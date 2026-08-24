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

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

const (
	artifactWatchDebounce = 300 * time.Millisecond
	artifactWatchPoll     = 5 * time.Second
)

// ArtifactWatcherMetaLifecycle describes the watcher meta-process lifecycle.
type ArtifactWatcherMetaLifecycle string

const (
	ArtifactWatcherMetaStarting   ArtifactWatcherMetaLifecycle = "starting"
	ArtifactWatcherMetaRunning    ArtifactWatcherMetaLifecycle = "running"
	ArtifactWatcherMetaRestarting ArtifactWatcherMetaLifecycle = "restarting"
	ArtifactWatcherMetaStopped    ArtifactWatcherMetaLifecycle = "stopped"
)

// artifactWatcherMetaState tracks the watcher meta-process state and restart policy.
type artifactWatcherMetaState struct {
	alias   gen.Alias
	restart *runtime.ScheduledBackoff
	status  artifactWatcherMetaStatus
}

// artifactWatcherMetaStatus is owned by reconcilerActor. The watcher
// meta-process reports directory facts; the actor derives lifecycle and
// availability and owns restart state.
type artifactWatcherMetaStatus struct {
	lifecycle    ArtifactWatcherMetaLifecycle
	availability runtime.Availability
}

// artifactWatcherMeta owns one watcher instance. fsnotify provides
// low-latency notifications and the periodic metadata fingerprint is a fallback
// for filesystems or mount implementations that miss an event. A temporarily
// absent directory is treated as artifact drift, not as a fatal watcher error;
// the meta-process keeps polling and reattaches fsnotify when the directory
// returns. The parent actor owns restart policy and fences notifications by
// alias.
type artifactWatcherMeta struct {
	gen.MetaProcess
	directory string
	runCtx    context.Context
	cancelRun context.CancelFunc
}

// artifactWatcherRunState tracks the watcher and its last published state.
type artifactWatcherRunState struct {
	watcher           *fsnotify.Watcher
	fingerprint       [sha256.Size]byte
	directoryReadable bool
	watchingDirectory bool
	statePublished    bool
	publishedReadable bool
	publishedWatching bool
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageArtifactDirectoryChanged reports possible filesystem drift.
type MessageArtifactDirectoryChanged struct{ source gen.Alias }

// MessageArtifactWatcherStateChanged reports watcher readability and attachment state.
type MessageArtifactWatcherStateChanged struct {
	source            gen.Alias
	directoryReadable bool
	watchingDirectory bool
}

// ---------------------------------------------------------------------------
// Meta lifecycle
// ---------------------------------------------------------------------------

// Init validates the watcher directory and initializes its cancellation context.
func (m *artifactWatcherMeta) Init(process gen.MetaProcess) error {
	if m.directory == "" {
		return fmt.Errorf("artifact watcher meta: directory is required")
	}
	m.MetaProcess = process
	m.runCtx, m.cancelRun = context.WithCancel(context.Background())
	return nil
}

// Start attaches the watcher and runs its filesystem change-detection loop.
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
		m.Log().Warning("artifact watcher unavailable: directory=%q alias=%s error=%v", m.directory, m.ID(), err)
	}
	if fingerprint, err := artifactDirectoryFingerprint(m.directory); err == nil {
		state.fingerprint = fingerprint
		state.directoryReadable = true
	} else {
		state.directoryReadable = false
		state.watchingDirectory = false
		m.Log().Warning("artifact watcher unavailable: directory=%q alias=%s error=%v", m.directory, m.ID(), err)
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
				if err := m.publishWatchError(&state, fmt.Errorf("%w: directory watch invalidated for %q: %s", runtime.ErrArtifactWatch, m.directory, event.Op)); err != nil {
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
			// An fsnotify event says to look, not what to conclude: executing a plugin binary
			// produces one on macOS, so the fingerprint decides as it does on a poll tick.
			debounceC = nil
			if err := m.notifyOnDirectoryChange(&state); err != nil {
				return err
			}

		case <-poll.C:
			if err := m.notifyOnDirectoryChange(&state); err != nil {
				return err
			}
		}
	}
}

// Terminate cancels the watcher change-detection loop.
func (m *artifactWatcherMeta) Terminate(error) {
	if m.cancelRun != nil {
		m.cancelRun()
	}
}

// ---------------------------------------------------------------------------
// Message handling
// ---------------------------------------------------------------------------

// HandleMessage ignores asynchronous messages because the watcher receives none.
func (m *artifactWatcherMeta) HandleMessage(gen.PID, any) error { return nil }

// HandleCall rejects synchronous calls because the watcher exposes no call API.
func (m *artifactWatcherMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("actorruntime: unsupported artifact watcher call %T", request), nil
}

// HandleInspect exposes no watcher inspection fields.
func (m *artifactWatcherMeta) HandleInspect(gen.PID, ...string) map[string]string { return nil }

// ---------------------------------------------------------------------------
// Watcher operations
// ---------------------------------------------------------------------------

// notifyOnDirectoryChange republishes watcher state and notifies the reconciler only when the
// directory fingerprint moved. Both change-detection paths decide here so they cannot diverge.
func (m *artifactWatcherMeta) notifyOnDirectoryChange(state *artifactWatcherRunState) error {
	fingerprint, err := artifactDirectoryFingerprint(m.directory)
	if err != nil {
		// An unreadable directory invalidates the resolution; notify once on the transition.
		wasReadable := state.directoryReadable
		state.directoryReadable = false
		state.watchingDirectory = false
		if err := m.publishWatchError(state, fmt.Errorf("%w: fingerprint directory %q: %w", runtime.ErrArtifactWatch, m.directory, err)); err != nil {
			return err
		}
		if !wasReadable {
			return nil
		}
	} else {
		wasReadable := state.directoryReadable
		state.directoryReadable = true
		if err := m.tryAttachWatch(state); err != nil {
			state.watchingDirectory = false
			if err := m.publishWatchError(state, err); err != nil {
				return err
			}
		} else if err := m.publishWatchState(state); err != nil {
			return err
		}
		if wasReadable && fingerprint == state.fingerprint {
			return nil
		}
		state.fingerprint = fingerprint
	}

	if err := m.Send(m.Parent(), MessageArtifactDirectoryChanged{source: m.ID()}); err != nil {
		return fmt.Errorf("%w: notify directory change: %w", runtime.ErrArtifactWatch, err)
	}
	return nil
}

// tryAttachWatch attaches fsnotify to the configured directory when needed.
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

// publishWatchError logs a watcher error when state changes and publishes the state.
func (m *artifactWatcherMeta) publishWatchError(state *artifactWatcherRunState, err error) error {
	if !state.statePublished ||
		state.publishedReadable != state.directoryReadable ||
		state.publishedWatching != state.watchingDirectory {
		m.Log().Warning("artifact watcher unavailable: directory=%q alias=%s error=%v", m.directory, m.ID(), err)
	}
	return m.publishWatchState(state)
}

// publishWatchState publishes changed watcher readability and attachment state.
func (m *artifactWatcherMeta) publishWatchState(state *artifactWatcherRunState) error {
	if state.statePublished &&
		state.publishedReadable == state.directoryReadable &&
		state.publishedWatching == state.watchingDirectory {
		return nil
	}

	if err := m.Send(m.Parent(), MessageArtifactWatcherStateChanged{
		source:            m.ID(),
		directoryReadable: state.directoryReadable,
		watchingDirectory: state.watchingDirectory,
	}); err != nil {
		return fmt.Errorf("%w: publish watcher state: %w", runtime.ErrArtifactWatch, err)
	}

	state.statePublished = true
	state.publishedReadable = state.directoryReadable
	state.publishedWatching = state.watchingDirectory
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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
