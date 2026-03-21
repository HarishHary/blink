package pluginmgr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/logger"
)

// Plugin is implemented by every plugin Manager - it can be started.
type Plugin interface {
	Start(ctx context.Context) error
}

// ISyncable is the type constraint for all plugin types managed by a Manager.
type ISyncable interface {
	Name() string
	Description() string
	Enabled() bool
	Checksum() string
}

// PluginLifecycle provides the health-check and graceful-shutdown primitives the Manager uses in ping loops and kill paths.
type PluginLifecycle interface {
	Ping(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// PluginHandle tracks everything the Manager needs for one running plugin subprocess.
type PluginHandle struct {
	Client    *plugin.Client
	Lifecycle PluginLifecycle
	BinPath   string
	ID        string // stable plugin identifier (e.g. UUID); used for bus messages and pool ops
	Name      string // human-readable display name; used for logging
	Hash      string // SHA-256 of the binary at launch time
	killOnce  sync.Once
	stopped   chan struct{}
}

// PluginAdapter[T] encapsulates every piece of type-specific plugin logic.
// Implement once per plugin type and inject into NewManager.
type PluginAdapter[T ISyncable] interface {
	// This is the go-plugin dispense key, e.g. "rule", "enrichment".
	PluginKey() string
	// This is the HandshakeConfig cookie value, e.g. "rule_v1".
	MagicValue() string
	// GRPCPlugin returns the go-plugin.Plugin that constructs the gRPC client stub.
	GRPCPlugin() plugin.Plugin
	// Handshake type-asserts the dispensed raw interface, calls Init (and optionally GetMetadata),
	// and returns the wrapped public T, a PluginLifecycle, the plugin stable ID, the display name, and any error.
	Handshake(ctx context.Context, raw interface{}, binPath string, hash string) (T, PluginLifecycle, string, string, error)
	// IsEnabled reports whether a running handle should continue running.
	IsEnabled(handle *PluginHandle) bool
	// Returns how many subprocess instances to spawn for this binary.
	// Return 1 (or ≤ 0) for the default single-worker behaviour.
	Workers(binPath string) int
}

// startFailure tracks consecutive start failures for a binary path.
type startFailure struct {
	count     int
	nextRetry time.Time
	hash      string // hash at time of last failure; reset backoff if binary changes
}

// PluginManager[T] is the generic plugin subprocess manager.
// It watches a directory for executable binaries, manages their subprocess lifecycle, and calls notify for Register/Update/Unregister events so the caller can update pools.
type PluginManager[T ISyncable] struct {
	log            *logger.Logger
	notify         Notify
	dir            string
	adapter        PluginAdapter[T]
	metrics        *PluginManagerMetrics
	mu             sync.RWMutex
	plugin_handles map[string][]*PluginHandle
	failures       map[string]*startFailure
	restarting     map[string]struct{} // paths mid-restart; reconcile skips these to prevent double-start
}

func NewPluginManager[T ISyncable](
	log *logger.Logger,
	notify Notify,
	dir string,
	adapter PluginAdapter[T],
	metrics *PluginManagerMetrics,
) *PluginManager[T] {
	return &PluginManager[T]{
		log:            log,
		notify:         notify,
		dir:            dir,
		adapter:        adapter,
		metrics:        metrics,
		plugin_handles: make(map[string][]*PluginHandle),
		failures:       make(map[string]*startFailure),
		restarting:     make(map[string]struct{}),
	}
}

// Performs an initial reconcile then watches the plugin directory for changes.
func (m *PluginManager[T]) Start(ctx context.Context) error {
	if err := m.reconcile("initial"); err != nil {
		return err
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(m.dir); err != nil {
		w.Close()
		return err
	}

	go func() {
		defer w.Close()
		var timer *time.Timer
		debounce := 400 * time.Millisecond
		// Periodic fallback: on macOS/kqueue, REMOVE events may not fire while a running
		// subprocess holds the binary's fd open. A 5-second poll catches those gaps,
		// and also picks up YAML sidecar changes that disable/remove rules.
		poll := time.NewTicker(5 * time.Second)
		defer poll.Stop()

		trigger := func(reason string) {
			if err := m.reconcile(reason); err != nil {
				m.log.ErrorF("reconcile error: %v", err)
			}
		}

		for {
			select {
			case evt, ok := <-w.Events:
				if !ok {
					return
				}
				info, _ := os.Stat(evt.Name)
				if info != nil && info.Mode()&0111 == 0 {
					continue // skip non-executables
				}
				// AfterFunc timers have no drainable C channel - just Stop and replace.
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(debounce, func() { trigger("debounce") })
			case <-poll.C:
				trigger("poll")
			case err := <-w.Errors:
				m.log.ErrorF("fsnotify error: %v", err)
				trigger("overflow")
			case <-ctx.Done():
				return
			}
		}
	}()

	return nil
}

func (m *PluginManager[T]) reconcile(reason string) error {
	m.log.Info("reconciling %s plugins (%s)...", m.adapter.PluginKey(), reason)

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(m.dir, e.Name())
		info, err := e.Info()
		if err != nil || info.Mode()&0111 == 0 {
			continue // skip non-executables
		}
		h, err := helpers.BinaryChecksum(path)
		if err != nil {
			m.log.ErrorF("hash %s: %v", path, err)
			continue
		}
		seen[path] = struct{}{}

		m.mu.RLock()
		handles, exists := m.plugin_handles[path]
		_, pending := m.restarting[path]
		m.mu.RUnlock()

		if pending {
			continue // pingLoop is already handling the restart
		}

		if exists {
			if handles[0].Hash == h {
				continue // binary unchanged
			}
			if err := m.update(path, handles, h); err != nil {
				m.log.ErrorF("update %s %s: %v", m.adapter.PluginKey(), path, err)
			}
			continue
		}

		if err := m.startWithBackoff(path, h); err != nil {
			m.log.ErrorF("start %s %s: %v", m.adapter.PluginKey(), path, err)
		}
	}

	// Collect plugins that need to be stopped or removed, then act outside the lock
	// so that kill() (gRPC Shutdown, up to 3s) does not block readers.
	type pendingAction struct {
		key     string
		handles []*PluginHandle
		perm    bool // true = binary deleted (remove); false = disabled (stop)
	}
	var pending []pendingAction
	m.mu.RLock()
	for key, handles := range m.plugin_handles {
		_, present := seen[key]
		if !present {
			pending = append(pending, pendingAction{key, handles, true})
		} else if !m.adapter.IsEnabled(handles[0]) {
			pending = append(pending, pendingAction{key, handles, false})
		}
	}
	m.mu.RUnlock()

	for _, p := range pending {
		if p.perm {
			m.remove(p.key, p.handles)
		} else {
			m.stop(p.key, p.handles)
		}
	}
	return nil
}

// This is a gRPC service config that retries UNAVAILABLE responses with exponential backoff. This absorbs the startup race where the subprocess hasn't yet
// bound its port when the first RPC arrives. maxAttempts=3 means 1 attempt + 2 retries.
const pluginRetryPolicy = `{
  "methodConfig": [{
    "name": [{}],
    "retryPolicy": {
      "maxAttempts": 3,
      "initialBackoff": "0.1s",
      "maxBackoff": "1s",
      "backoffMultiplier": 2.0,
      "retryableStatusCodes": ["UNAVAILABLE"]
    }
  }]
}`

// spawn ONE subprocess, runs the PluginAdapter handshake, and returns the
// wrapped handle. It does NOT store the handle in plugin_handles or start pingLoop -
// spawnN handles that after all worker instances are ready.
func (m *PluginManager[T]) spawn(path, hash string) (T, *PluginHandle, error) {
	startedAt := time.Now()

	cfg := &plugin.ClientConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  1,
			MagicCookieKey:   "BLINK_PLUGIN",
			MagicCookieValue: m.adapter.MagicValue(),
		},
		Cmd:              exec.Command(path),
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Plugins: map[string]plugin.Plugin{
			m.adapter.PluginKey(): m.adapter.GRPCPlugin(),
		},
		GRPCDialOptions: []grpc.DialOption{
			grpc.WithDefaultServiceConfig(pluginRetryPolicy),
		},
	}

	cl := plugin.NewClient(cfg)
	rpcClient, err := cl.Client()
	if err != nil {
		cl.Kill()
		var zero T
		return zero, nil, fmt.Errorf("connect: %w", err)
	}

	raw, err := rpcClient.Dispense(m.adapter.PluginKey())
	if err != nil {
		cl.Kill()
		var zero T
		return zero, nil, fmt.Errorf("dispense: %w", err)
	}

	wrapped, lifecycle, id, name, err := m.adapter.Handshake(context.Background(), raw, path, hash)
	if err != nil {
		cl.Kill()
		var zero T
		return zero, nil, err
	}

	handle := &PluginHandle{Client: cl, Lifecycle: lifecycle, BinPath: path, ID: id, Name: name, Hash: hash, stopped: make(chan struct{})}

	m.metrics.StartLatency.Observe(time.Since(startedAt).Seconds())
	m.metrics.ActiveSubprocesses.WithLabelValues(m.adapter.PluginKey()).Inc()
	m.metrics.Starts.Inc()
	m.log.Info("%s started: %s [%s] (%s)", m.adapter.PluginKey(), name, id, path)

	return wrapped, handle, nil
}

// spawnN spawns n worker subprocess instances for the same binary, stores the full
// slice in plugin_handles, and starts a pingLoop for each. If any spawn fails, all
// already-started subprocesses are killed and an error is returned.
func (m *PluginManager[T]) spawnN(path, hash string, n int) ([]T, []*PluginHandle, error) {
	if n <= 0 {
		n = 1
	}
	wrapped := make([]T, 0, n)
	handles := make([]*PluginHandle, 0, n)

	for i := 0; i < n; i++ {
		w, h, err := m.spawn(path, hash)
		if err != nil {
			for _, h := range handles {
				m.kill(h)
			}
			return nil, nil, err
		}
		wrapped = append(wrapped, w)
		handles = append(handles, h)
	}

	m.mu.Lock()
	m.plugin_handles[path] = handles
	m.mu.Unlock()

	for _, h := range handles {
		go m.pingLoop(h)
	}
	return wrapped, handles, nil
}

// wraps start() with exponential backoff on consecutive failures.
func (m *PluginManager[T]) startWithBackoff(path, hash string) error {
	m.mu.Lock()
	f := m.failures[path]
	if f != nil {
		if f.hash != hash {
			// Binary changed — reset backoff immediately.
			delete(m.failures, path)
			f = nil
		} else if time.Now().Before(f.nextRetry) {
			m.mu.Unlock()
			m.log.Info("%s %s start deferred (backoff, retry in %v)", m.adapter.PluginKey(), path, time.Until(f.nextRetry).Round(time.Second))
			return nil
		}
	}
	m.mu.Unlock()

	err := m.start(path, hash)
	if err != nil {
		m.mu.Lock()
		f = m.failures[path]
		if f == nil {
			f = &startFailure{hash: hash}
			m.failures[path] = f
		}
		f.count++
		backoff := time.Duration(10<<min(f.count-1, 5)) * time.Second // 10s→320s, cap 5min
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		f.nextRetry = time.Now().Add(backoff)
		m.mu.Unlock()
		m.log.ErrorF("%s %s start failed (attempt %d), next retry in %v", m.adapter.PluginKey(), path, f.count, backoff)
		return err
	}

	// Success — clear any failure state.
	m.mu.Lock()
	delete(m.failures, path)
	m.mu.Unlock()
	return nil
}

// spawns n worker subprocesses and notifies the pool to register them.
func (m *PluginManager[T]) start(path, hash string) error {
	n := m.adapter.Workers(path)
	wrapped, handles, err := m.spawnN(path, hash, n)
	if err != nil {
		return err
	}
	m.notify(NewRegisterMessage[T](wrapped, len(handles)))
	return nil
}

// spawns new worker subprocesses and notifies the pool with an onDrained callback.
// The old subprocesses are only killed after all in-flight calls on the old VersionedPool
// complete - ensuring no call ever hits a dead gRPC connection.
func (m *PluginManager[T]) update(path string, oldHandles []*PluginHandle, newHash string) error {
	n := m.adapter.Workers(path)
	wrapped, newHandles, err := m.spawnN(path, newHash, n)
	if err != nil {
		return err
	}
	m.notify(NewUpdateMessage[T](wrapped, len(newHandles), func() {
		for _, h := range oldHandles {
			m.kill(h)
		}
	}))
	m.metrics.Updates.Inc()
	m.log.Info("%s updated: %s (%d worker(s))", m.adapter.PluginKey(), path, len(newHandles))
	return nil
}

// kill gracefully shuts down the subprocess exactly once (safe for concurrent calls).
// It does NOT touch plugin_handles - callers that own the map entry call evict instead.
func (m *PluginManager[T]) kill(handle *PluginHandle) {
	handle.killOnce.Do(func() {
		close(handle.stopped)
		defer func() { recover() }() //nolint:errcheck - best-effort shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = handle.Lifecycle.Shutdown(ctx)
		cancel()
		handle.Client.Kill()
		m.metrics.ActiveSubprocesses.WithLabelValues(m.adapter.PluginKey()).Dec()
	})
}

// kills all handles in the group and removes the group from plugin_handles.
// It acquires the write lock only for the map delete, so kill() (gRPC Shutdown)
// runs outside the lock. Guards against a concurrent handle replacement at the same key
// by checking that the stored slice still begins with the same pointer.
func (m *PluginManager[T]) evict(key string, handles []*PluginHandle) {
	for _, h := range handles {
		m.kill(h)
	}
	m.mu.Lock()
	current := m.plugin_handles[key]
	if len(current) > 0 && len(handles) > 0 && current[0] == handles[0] {
		delete(m.plugin_handles, key)
	}
	m.mu.Unlock()
}

// evicts the subprocesses transiently (crash restart, config disable) and
// sends UnregisterMessage - pool removes the active entry but does NOT tombstone.
func (m *PluginManager[T]) stop(key string, handles []*PluginHandle) {
	m.evict(key, handles)
	m.notify(NewUnregisterMessage[T](handles[0].ID))
	m.log.Info("%s stopped: %s [%s]", m.adapter.PluginKey(), handles[0].Name, handles[0].ID)
}

// evicts the subprocesses permanently (binary deleted from disk) and
// sends RemoveMessage - pool removes the active entry AND tombstones the plugin ID.
func (m *PluginManager[T]) remove(key string, handles []*PluginHandle) {
	m.evict(key, handles)
	m.notify(NewRemoveMessage[T](handles[0].ID))
	m.log.Info("%s removed: %s [%s]", m.adapter.PluginKey(), handles[0].Name, handles[0].ID)
}

// stops the subprocesses and restarts them with backoff.
// Sets restarting[path] before stop() so reconcile() does not race to fill the
// now-empty plugin_handles slot while the new spawn is in progress.
func (m *PluginManager[T]) restart(key string, handles []*PluginHandle) error {
	path := handles[0].BinPath
	hash := handles[0].Hash

	m.mu.Lock()
	m.restarting[path] = struct{}{}
	m.mu.Unlock()

	m.stop(key, handles)

	err := m.startWithBackoff(path, hash)

	m.mu.Lock()
	delete(m.restarting, path)
	m.mu.Unlock()

	return err
}

func (m *PluginManager[T]) pingLoop(handle *PluginHandle) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-handle.stopped:
			return // intentionally stopped - do not restart
		case <-t.C:
			// During a graceful update, spawnN stores the new handles in the map
			// before notify() is called. If this handle is no longer in the active
			// slice, it was replaced - exit without restarting.
			m.mu.RLock()
			current := m.plugin_handles[handle.BinPath]
			m.mu.RUnlock()
			active := false
			for _, h := range current {
				if h == handle {
					active = true
					break
				}
			}
			if !active {
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := handle.Lifecycle.Ping(ctx)
			cancel()
			if err != nil {
				m.metrics.Crashes.Inc()
				m.log.ErrorF("%s crash/health fail %s: %v - restarting", m.adapter.PluginKey(), handle.Name, err)
				// Fetch the full current group so restart kills all workers, not just this one.
				m.mu.RLock()
				group := m.plugin_handles[handle.BinPath]
				m.mu.RUnlock()
				if restartErr := m.restart(handle.BinPath, group); restartErr != nil {
					m.log.Error(errors.NewF("restart failed for %s: %v", handle.BinPath, restartErr))
				}
				m.metrics.Restarts.Inc()
				return
			}
		}
	}
}
