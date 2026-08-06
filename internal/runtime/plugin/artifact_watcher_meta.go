package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
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
	Incarnation       uint64
	RestartCount      uint64
	RestartPending    bool
	DirectoryReadable bool
	WatchingDirectory bool
	LastError         error
}

// artifactWatcherMeta owns one watcher incarnation. fsnotify provides
// low-latency notifications and the periodic metadata fingerprint is a fallback
// for filesystems or mount implementations that miss an event. A temporarily
// absent directory is treated as artifact drift, not as a fatal watcher error;
// the meta-process keeps polling and reattaches fsnotify when the directory
// returns. The parent actor owns restart policy and fences notifications by
// incarnation.
type artifactWatcherMeta struct {
	gen.MetaProcess

	directory   string
	incarnation uint64
	runCtx      context.Context
	cancelRun   context.CancelFunc
}

type artifactWatcherRunState struct {
	watcher             *fsnotify.Watcher
	fingerprint         [sha256.Size]byte
	directoryReadable   bool
	watchingDirectory   bool
	watchError          error
	statePublished      bool
	publishedReadable   bool
	publishedWatching   bool
	publishedWatchError string
}

func (m *artifactWatcherMeta) Init(process gen.MetaProcess) error {
	if m.directory == "" {
		return fmt.Errorf("artifact watcher meta: directory is required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	return nil
}

func (m *artifactWatcherMeta) Start() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("%w: create watcher: %w", runtime.ErrArtifactWatch, err)
	}
	defer watcher.Close()
	state := artifactWatcherRunState{watcher: watcher}

	// Directory availability is external. Keep polling when the mount or
	// directory is temporarily absent instead of failing this process.
	if err := m.tryAttachWatch(&state); err != nil {
		state.watchError = err
	}
	if fingerprint, err := artifactDirectoryFingerprint(m.directory); err == nil {
		state.fingerprint = fingerprint
		state.directoryReadable = true
	} else {
		state.directoryReadable = false
		state.watchingDirectory = false
		state.watchError = fmt.Errorf("%w: fingerprint directory %q: %w", runtime.ErrArtifactWatch, m.directory, err)
	}

	if err := m.Send(m.Parent(), MessageArtifactWatcherStarted{incarnation: m.incarnation}); err != nil {
		return fmt.Errorf("%w: announce start: %w", runtime.ErrArtifactWatch, err)
	}
	if err := m.publishWatchState(&state); err != nil {
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

		case event, ok := <-state.watcher.Events:
			if !ok {
				if m.runCtx.Err() != nil {
					return nil
				}
				return fmt.Errorf("%w: event channel closed", runtime.ErrArtifactWatch)
			}
			if filepath.Clean(event.Name) == filepath.Clean(m.directory) &&
				event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				state.watchingDirectory = false
				if err := m.recordWatchError(&state, fmt.Errorf(
					"%w: directory watch invalidated for %q: %s",
					runtime.ErrArtifactWatch,
					m.directory,
					event.Op,
				)); err != nil {
					return err
				}
			}
			scheduleNotification()

		case err, ok := <-state.watcher.Errors:
			if !ok {
				if m.runCtx.Err() != nil {
					return nil
				}
				return fmt.Errorf("%w: error channel closed", runtime.ErrArtifactWatch)
			}
			if m.runCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("%w: %w", runtime.ErrArtifactWatch, err)

		case <-debounceC:
			debounceC = nil
			// An fsnotify event itself is sufficient evidence of possible drift.
			// Refresh the poll baseline when possible, then trigger a full resolve.
			if fingerprint, err := artifactDirectoryFingerprint(m.directory); err == nil {
				state.fingerprint = fingerprint
				state.directoryReadable = true
				if err := m.tryAttachWatch(&state); err != nil {
					state.watchingDirectory = false
					if err := m.recordWatchError(&state, err); err != nil {
						return err
					}
				} else if err := m.clearWatchError(&state); err != nil {
					return err
				}
			} else {
				state.directoryReadable = false
				state.watchingDirectory = false
				if err := m.recordWatchError(&state, fmt.Errorf(
					"%w: fingerprint directory %q: %w",
					runtime.ErrArtifactWatch,
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
				wasReadable := state.directoryReadable
				state.directoryReadable = false
				state.watchingDirectory = false
				if err := m.recordWatchError(&state, fmt.Errorf(
					"%w: fingerprint directory %q: %w",
					runtime.ErrArtifactWatch,
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

			wasReadable := state.directoryReadable
			state.directoryReadable = true
			if err := m.tryAttachWatch(&state); err != nil {
				state.watchingDirectory = false
				if err := m.recordWatchError(&state, err); err != nil {
					return err
				}
			} else if err := m.clearWatchError(&state); err != nil {
				return err
			}

			if wasReadable && fingerprint == state.fingerprint {
				continue
			}
			state.fingerprint = fingerprint
			if err := m.notifyChanged(); err != nil {
				return err
			}
		}
	}
}

func (m *artifactWatcherMeta) HandleMessage(gen.PID, any) error { return nil }

func (m *artifactWatcherMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return nil, fmt.Errorf("actorruntime: unsupported artifact watcher call %T", request)
}

func (m *artifactWatcherMeta) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

func (m *artifactWatcherMeta) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"directory":   m.directory,
		"incarnation": fmt.Sprintf("%d", m.incarnation),
	}
}

func (m *artifactWatcherMeta) notifyChanged() error {
	if err := m.Send(m.Parent(), MessageArtifactDirectoryChanged{
		incarnation: m.incarnation,
	}); err != nil {
		return fmt.Errorf("%w: notify directory change: %w", runtime.ErrArtifactWatch, err)
	}
	return nil
}

func (m *artifactWatcherMeta) tryAttachWatch(state *artifactWatcherRunState) error {
	if state.watchingDirectory {
		return nil
	}
	if err := state.watcher.Add(m.directory); err != nil {
		state.watchingDirectory = false
		return fmt.Errorf("%w: watch directory %q: %w", runtime.ErrArtifactWatch, m.directory, err)
	}
	state.watchingDirectory = true
	return nil
}

func (m *artifactWatcherMeta) recordWatchError(state *artifactWatcherRunState, err error) error {
	state.watchError = err
	return m.publishWatchState(state)
}

func (m *artifactWatcherMeta) clearWatchError(state *artifactWatcherRunState) error {
	state.watchError = nil
	return m.publishWatchState(state)
}

func (m *artifactWatcherMeta) publishWatchState(state *artifactWatcherRunState) error {
	watchError := errorText(state.watchError)
	if state.statePublished &&
		state.publishedReadable == state.directoryReadable &&
		state.publishedWatching == state.watchingDirectory &&
		state.publishedWatchError == watchError {
		return nil
	}

	if err := m.Send(m.Parent(), MessageArtifactWatcherStateChanged{
		incarnation:       m.incarnation,
		directoryReadable: state.directoryReadable,
		watchingDirectory: state.watchingDirectory,
		err:               state.watchError,
	}); err != nil {
		return fmt.Errorf("%w: publish watcher state: %w", runtime.ErrArtifactWatch, err)
	}

	state.statePublished = true
	state.publishedReadable = state.directoryReadable
	state.publishedWatching = state.watchingDirectory
	state.publishedWatchError = watchError
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
