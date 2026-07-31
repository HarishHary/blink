package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"ergo.services/ergo/gen"
	"github.com/fsnotify/fsnotify"
	"github.com/harishhary/blink/internal/runtime"
)

const (
	artifactWatchDebounce = 300 * time.Millisecond
	artifactWatchPoll     = 5 * time.Second
)

// ArtifactWatcherLifecycle describes one watcher meta-process incarnation.
type ArtifactWatcherLifecycle string

const (
	ArtifactWatcherStarting   ArtifactWatcherLifecycle = "starting"
	ArtifactWatcherRunning    ArtifactWatcherLifecycle = "running"
	ArtifactWatcherRestarting ArtifactWatcherLifecycle = "restarting"
	ArtifactWatcherStopped    ArtifactWatcherLifecycle = "stopped"
)

// ArtifactWatcherStatus is owned by desiredStateReconcilerActor. The watcher
// meta-process reports directory facts; the actor derives lifecycle and
// availability and owns restart state.
type ArtifactWatcherStatus struct {
	Lifecycle         ArtifactWatcherLifecycle
	Availability      runtime.Availability
	Generation        uint64
	RestartCount      uint64
	RestartPending    bool
	DirectoryReadable bool
	WatchingDirectory bool
	LastError         string
}

// artifactWatcherMeta owns one watcher incarnation. fsnotify provides
// low-latency notifications and the periodic metadata fingerprint is a fallback
// for filesystems or mount implementations that miss an event. A temporarily
// absent directory is treated as artifact drift, not as a fatal watcher error;
// the meta-process keeps polling and reattaches fsnotify when the directory
// returns. The parent actor owns restart policy and fences notifications by
// generation.
type artifactWatcherMeta struct {
	gen.MetaProcess

	directory  string
	generation uint64
	watcher    *fsnotify.Watcher
	runCtx     context.Context
	cancelRun  context.CancelFunc
	closeOnce  sync.Once

	fingerprint       [sha256.Size]byte
	directoryReadable atomic.Bool
	watchingDirectory atomic.Bool

	stateMu             sync.RWMutex
	watchError          string
	statePublished      bool
	publishedReadable   bool
	publishedWatching   bool
	publishedWatchError string
}

func (m *artifactWatcherMeta) Init(process gen.MetaProcess) error {
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create artifact watcher: %w", err)
	}
	m.watcher = watcher

	// Directory availability is an external condition. Do not fail process
	// initialization when the mount or directory is temporarily absent.
	if err := m.tryAttachWatch(); err != nil {
		m.watchingDirectory.Store(false)
		m.setWatchError(err)
	}
	if fingerprint, err := artifactDirectoryFingerprint(m.directory); err == nil {
		m.fingerprint = fingerprint
		m.directoryReadable.Store(true)
	} else {
		m.directoryReadable.Store(false)
		m.setWatchError(fmt.Errorf("fingerprint artifact directory %q: %w", m.directory, err))
	}
	return nil
}

func (m *artifactWatcherMeta) Start() error {
	if err := m.Send(m.Parent(), artifactWatcherStarted{generation: m.generation}); err != nil {
		return fmt.Errorf("announce artifact watcher start: %w", err)
	}
	if err := m.publishWatchState(); err != nil {
		return err
	}

	poll := time.NewTicker(artifactWatchPoll)
	defer poll.Stop()

	var debounce *time.Timer
	var debounceC <-chan time.Time

	scheduleNotification := func() {
		if debounce == nil {
			debounce = time.NewTimer(artifactWatchDebounce)
			debounceC = debounce.C
			return
		}
		if !debounce.Stop() {
			select {
			case <-debounce.C:
			default:
			}
		}
		debounce.Reset(artifactWatchDebounce)
		debounceC = debounce.C
	}

	for {
		select {
		case <-m.runCtx.Done():
			if debounce != nil {
				debounce.Stop()
			}
			return nil

		case event, ok := <-m.watcher.Events:
			if !ok {
				if m.runCtx.Err() != nil {
					return nil
				}
				return fmt.Errorf("artifact watcher event channel closed")
			}
			if filepath.Clean(event.Name) == filepath.Clean(m.directory) &&
				event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				m.watchingDirectory.Store(false)
				if err := m.recordWatchError(fmt.Errorf(
					"artifact directory watch invalidated for %q: %s",
					m.directory,
					event.Op,
				)); err != nil {
					return err
				}
			}
			scheduleNotification()

		case err, ok := <-m.watcher.Errors:
			if !ok {
				if m.runCtx.Err() != nil {
					return nil
				}
				return fmt.Errorf("artifact watcher error channel closed")
			}
			if m.runCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("artifact watcher: %w", err)

		case <-debounceC:
			debounceC = nil
			// An fsnotify event itself is sufficient evidence of possible drift.
			// Refresh the poll baseline when possible, then trigger a full resolve.
			if fingerprint, err := artifactDirectoryFingerprint(m.directory); err == nil {
				m.fingerprint = fingerprint
				m.directoryReadable.Store(true)
				if err := m.tryAttachWatch(); err != nil {
					m.watchingDirectory.Store(false)
					if err := m.recordWatchError(err); err != nil {
						return err
					}
				} else if err := m.clearWatchError(); err != nil {
					return err
				}
			} else {
				m.directoryReadable.Store(false)
				m.watchingDirectory.Store(false)
				if err := m.recordWatchError(fmt.Errorf(
					"fingerprint artifact directory %q: %w",
					m.directory,
					err,
				)); err != nil {
					return err
				}
			}
			if err := m.notifyChanged(); err != nil {
				return err
			}

		case <-poll.C:
			fingerprint, err := artifactDirectoryFingerprint(m.directory)
			if err != nil {
				// Missing or unreadable directories invalidate the current resolution.
				// Notify only on the transition; the reconciler's backoff continues
				// retries until the directory recovers.
				wasReadable := m.directoryReadable.Swap(false)
				m.watchingDirectory.Store(false)
				if err := m.recordWatchError(fmt.Errorf(
					"fingerprint artifact directory %q: %w",
					m.directory,
					err,
				)); err != nil {
					return err
				}
				if wasReadable {
					if err := m.notifyChanged(); err != nil {
						return err
					}
				}
				continue
			}

			wasReadable := m.directoryReadable.Load()
			m.directoryReadable.Store(true)
			if err := m.tryAttachWatch(); err != nil {
				m.watchingDirectory.Store(false)
				if err := m.recordWatchError(err); err != nil {
					return err
				}
			} else if err := m.clearWatchError(); err != nil {
				return err
			}

			if wasReadable && fingerprint == m.fingerprint {
				continue
			}
			m.fingerprint = fingerprint
			if err := m.notifyChanged(); err != nil {
				return err
			}
		}
	}
}

func (m *artifactWatcherMeta) HandleMessage(gen.PID, any) error { return nil }

func (m *artifactWatcherMeta) HandleCall(gen.PID, gen.Ref, any) (any, error) {
	return nil, nil
}

func (m *artifactWatcherMeta) Terminate(error) {
	m.closeOnce.Do(func() {
		if m.cancelRun != nil {
			m.cancelRun()
		}
		if m.watcher != nil {
			_ = m.watcher.Close()
		}
	})
}

func (m *artifactWatcherMeta) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"directory":          m.directory,
		"generation":         fmt.Sprintf("%d", m.generation),
		"directory_readable": fmt.Sprintf("%t", m.directoryReadable.Load()),
		"watching_directory": fmt.Sprintf("%t", m.watchingDirectory.Load()),
		"last_watch_error":   m.currentWatchError(),
	}
}

func (m *artifactWatcherMeta) notifyChanged() error {
	if err := m.Send(m.Parent(), artifactDirectoryChanged{
		generation: m.generation,
	}); err != nil {
		return fmt.Errorf("notify artifact directory change: %w", err)
	}
	return nil
}

func (m *artifactWatcherMeta) tryAttachWatch() error {
	if m.watcher == nil || m.watchingDirectory.Load() {
		return nil
	}
	if err := m.watcher.Add(m.directory); err != nil {
		m.watchingDirectory.Store(false)
		return fmt.Errorf("watch artifact directory %q: %w", m.directory, err)
	}
	m.watchingDirectory.Store(true)
	return nil
}

func (m *artifactWatcherMeta) recordWatchError(err error) error {
	m.setWatchError(err)
	return m.publishWatchState()
}

func (m *artifactWatcherMeta) clearWatchError() error {
	m.setWatchError(nil)
	return m.publishWatchState()
}

func (m *artifactWatcherMeta) setWatchError(err error) {
	m.stateMu.Lock()
	m.watchError = errorText(err)
	m.stateMu.Unlock()
}

func (m *artifactWatcherMeta) currentWatchError() string {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.watchError
}

func (m *artifactWatcherMeta) publishWatchState() error {
	readable := m.directoryReadable.Load()
	watching := m.watchingDirectory.Load()
	watchError := m.currentWatchError()

	m.stateMu.Lock()
	if m.statePublished &&
		m.publishedReadable == readable &&
		m.publishedWatching == watching &&
		m.publishedWatchError == watchError {
		m.stateMu.Unlock()
		return nil
	}
	m.stateMu.Unlock()

	if err := m.Send(m.Parent(), artifactWatcherStateChanged{
		generation:        m.generation,
		directoryReadable: readable,
		watchingDirectory: watching,
		err:               watchError,
	}); err != nil {
		return fmt.Errorf("publish artifact watcher state: %w", err)
	}

	m.stateMu.Lock()
	m.statePublished = true
	m.publishedReadable = readable
	m.publishedWatching = watching
	m.publishedWatchError = watchError
	m.stateMu.Unlock()
	return nil
}

// artifactDirectoryFingerprint intentionally hashes metadata rather than file
// contents. It is only a change detector; artifactResolverMeta performs the
// authoritative SHA-256 content verification before a deployment is applied.
func artifactDirectoryFingerprint(directory string) ([sha256.Size]byte, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return [sha256.Size]byte{}, err
	}

	h := sha256.New()
	var number [8]byte
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return [sha256.Size]byte{}, err
		}

		_, _ = h.Write([]byte(filepath.Base(entry.Name())))
		_, _ = h.Write([]byte{0})
		binary.LittleEndian.PutUint64(number[:], uint64(info.Size()))
		_, _ = h.Write(number[:])
		binary.LittleEndian.PutUint64(number[:], uint64(info.ModTime().UnixNano()))
		_, _ = h.Write(number[:])
		binary.LittleEndian.PutUint64(number[:], uint64(info.Mode()))
		_, _ = h.Write(number[:])
	}

	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], h.Sum(nil))
	return fingerprint, nil
}
