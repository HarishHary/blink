package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
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

var ErrArtifactWatch = errors.New("plugin artifact watch failed")

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
	alias       gen.Alias
	statusEpoch int64
	restart     *runtime.ScheduledBackoff
	status      artifactWatcherMetaStatus
}

// artifactWatcherMetaStatus is owned by reconcilerActor: the watcher reports directory facts, the
// actor derives lifecycle and availability.
type artifactWatcherMetaStatus struct {
	lifecycle    ArtifactWatcherMetaLifecycle
	availability runtime.Availability
	err          error
}

// artifactWatcherMeta owns one watcher: fsnotify for latency, a periodic fingerprint for the events
// it misses, and polling through an absent directory, which is drift rather than a fatal error.
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
	lastStatus        MessageArtifactWatcherStatusChanged
	lastStatusEpoch   int64
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageArtifactDirectoryChanged reports possible filesystem drift.
type MessageArtifactDirectoryChanged struct{ source gen.Alias }

// MessageArtifactWatcherStatusChanged reports watcher readability and attachment state.
type MessageArtifactWatcherStatusChanged struct {
	source            gen.Alias
	statusEpoch       int64
	directoryReadable bool
	watchingDirectory bool
	err               error
}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
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
		return fmt.Errorf("%w: create watcher: %w", ErrArtifactWatch, err)
	}
	defer watcher.Close()
	state := artifactWatcherRunState{watcher: watcher}

	// Directory availability is external: keep polling rather than failing this process.
	watchErr := m.tryAttachWatch(&state)
	if err := watchErr; err != nil {
		m.Log().Warning("artifact watcher unavailable: directory=%q alias=%s error=%v", m.directory, m.ID(), err)
	}
	if fingerprint, err := artifactDirectoryFingerprint(m.directory); err == nil {
		state.fingerprint = fingerprint
		state.directoryReadable = true
	} else {
		state.directoryReadable = false
		state.watchingDirectory = false
		watchErr = fmt.Errorf("%w: fingerprint: %w", ErrArtifactWatch, err)
		m.Log().Warning("artifact watcher unavailable: directory=%q alias=%s error=%v", m.directory, m.ID(), err)
	}

	if err := m.reconcileStatus(&state, watchErr); err != nil {
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
				return fmt.Errorf("%w: event channel closed", ErrArtifactWatch)
			}
			if filepath.Clean(event.Name) == filepath.Clean(m.directory) &&
				event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				state.watchingDirectory = false
				if err := m.reconcileStatus(&state, fmt.Errorf("%w: directory watch invalidated for %q: %s", ErrArtifactWatch, m.directory, event.Op)); err != nil {
					return err
				}
			}
			scheduleNotification()

		case err, ok := <-state.watcher.Errors:
			if !ok {
				if m.runCtx.Err() != nil {
					return nil
				}
				return fmt.Errorf("%w: error channel closed", ErrArtifactWatch)
			}
			if m.runCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("%w: %w", ErrArtifactWatch, err)

		case <-debounceC:
			// An event says to look, not what to conclude; the fingerprint decides, as on a poll tick.
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

// HandleMessage ignores asynchronous messages because the watcher receives none.
func (m *artifactWatcherMeta) HandleMessage(gen.PID, any) error { return nil }

// HandleCall rejects synchronous calls because the watcher exposes no call API.
func (m *artifactWatcherMeta) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("unsupported artifact watcher call %T", request), nil
}

// HandleInspect exposes the watched directory and whether this instance is shutting down
func (m *artifactWatcherMeta) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"watcher:directory":     m.directory,
		"watcher:shutting_down": fmt.Sprintf("%t", m.runCtx != nil && m.runCtx.Err() != nil),
	}
}

// ---------------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------------

// notifyOnDirectoryChange notifies the reconciler only when the fingerprint moved, for both
// change-detection paths, so they cannot diverge.
func (m *artifactWatcherMeta) notifyOnDirectoryChange(state *artifactWatcherRunState) error {
	fingerprint, err := artifactDirectoryFingerprint(m.directory)
	if err != nil {
		// An unreadable directory invalidates the resolution; notify once on the transition.
		wasReadable := state.directoryReadable
		state.directoryReadable = false
		state.watchingDirectory = false
		if err := m.reconcileStatus(state, fmt.Errorf("%w: fingerprint directory %q: %w", ErrArtifactWatch, m.directory, err)); err != nil {
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
			if err := m.reconcileStatus(state, err); err != nil {
				return err
			}
		} else if err := m.reconcileStatus(state, nil); err != nil {
			return err
		}
		if wasReadable && fingerprint == state.fingerprint {
			return nil
		}
		state.fingerprint = fingerprint
	}

	if err := m.SendWithPriority(m.Parent(), MessageArtifactDirectoryChanged{source: m.ID()}, gen.MessagePriorityHigh); err != nil {
		return fmt.Errorf("%w: notify directory change: %w", ErrArtifactWatch, err)
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
		return fmt.Errorf("%w: watch directory %q: %w", ErrArtifactWatch, m.directory, err)
	}
	state.watchingDirectory = true
	return nil
}

// artifactDirectoryFingerprint hashes metadata, not contents: it only detects change, while
// artifactResolverMeta verifies content before a deployment is applied.
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

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// reconcileStatus propagates changed directory facts; the owner actor derives health and gauges.
// The cache stays in Start's run state, never in fields shared with concurrent meta callbacks.
func (m *artifactWatcherMeta) reconcileStatus(state *artifactWatcherRunState, watchErr error) error {
	next := MessageArtifactWatcherStatusChanged{
		statusEpoch:       runtime.NextStatusEpoch(state.lastStatusEpoch),
		source:            m.ID(),
		directoryReadable: state.directoryReadable,
		watchingDirectory: state.watchingDirectory,
		err:               watchErr,
	}
	if state.lastStatusEpoch != 0 && sameArtifactWatcherStatus(state.lastStatus, next) {
		return nil
	}
	if watchErr != nil {
		m.Log().Warning("artifact watcher unavailable: directory=%q alias=%s error=%v", m.directory, m.ID(), watchErr)
	}
	if err := m.propagateStatus(next); err != nil {
		return err
	}

	state.lastStatus = next
	state.lastStatusEpoch = next.statusEpoch
	return nil
}

// propagateStatus sends directory facts without changing the reconciliation cache.
func (m *artifactWatcherMeta) propagateStatus(next MessageArtifactWatcherStatusChanged) error {
	if err := m.SendWithPriority(m.Parent(), next, gen.MessagePriorityHigh); err != nil {
		return fmt.Errorf("%w: publish watcher state: %w", ErrArtifactWatch, err)
	}
	return nil
}

// sameArtifactWatcherStatus compares directory facts, independently of their publication epoch.
func sameArtifactWatcherStatus(left, right MessageArtifactWatcherStatusChanged) bool {
	return left.directoryReadable == right.directoryReadable &&
		left.watchingDirectory == right.watchingDirectory &&
		runtime.ErrorText(left.err) == runtime.ErrorText(right.err)
}
