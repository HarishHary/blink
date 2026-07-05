package plugin

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/handshake"
	"github.com/harishhary/blink/internal/pools"
)

// pluginRetryPolicy: gRPC config retrying UNAVAILABLE 3× (1 attempt + 2 retries) with backoff, absorbing the startup race before the subprocess binds its port.
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

// spawn starts ONE subprocess and runs the adapter handshake, returning the wrapped handle; it does not store it or start pingLoop (spawnN does, once all workers are ready).
func (m *PluginExecutor[T]) spawn(path, hash string) (T, *PluginHandle, error) {
	startedAt := time.Now()

	cfg := &goplugin.ClientConfig{
		HandshakeConfig: goplugin.HandshakeConfig{
			ProtocolVersion:  handshake.ProtocolVersion,
			MagicCookieKey:   handshake.CookieKey,
			MagicCookieValue: m.adapter.MagicValue(),
		},
		Cmd:              exec.Command(path),
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		Plugins: map[string]goplugin.Plugin{
			m.adapter.PluginKey(): m.adapter.GRPCPlugin(),
		},
		GRPCDialOptions: []grpc.DialOption{
			grpc.WithDefaultServiceConfig(pluginRetryPolicy),
		},
	}

	cl := goplugin.NewClient(cfg)
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

	handle := &PluginHandle{Client: cl, Lifecycle: lifecycle, BinPath: path, Key: pools.PoolKey{Id: id, Name: wrapped.Metadata().Name, Hash: hash}, Mode: wrapped.Metadata().RolloutMode, Name: name, stopped: make(chan struct{})}

	m.metrics.StartLatency.Observe(time.Since(startedAt).Seconds())
	m.metrics.ActiveSubprocesses.WithLabelValues(m.adapter.PluginKey()).Inc()
	m.metrics.Starts.Inc()
	m.logger.Info("%s started: %s [%s] (%s)", m.adapter.PluginKey(), name, id, path)

	return wrapped, handle, nil
}

// spawnN spawns n workers for the same binary, stores them in plugin_handles, and starts a pingLoop each; if any spawn fails, all already-started ones are killed (start is atomic).
func (m *PluginExecutor[T]) spawnN(path, hash string, n int) ([]T, []*PluginHandle, error) {
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

// startWithBackoff wraps start() with exponential backoff on consecutive failures.
func (m *PluginExecutor[T]) startWithBackoff(path, hash string) error {
	m.mu.Lock()
	f := m.failures[path]
	if f != nil {
		if f.hash != hash {
			// Binary changed - reset backoff immediately.
			delete(m.failures, path)
			f = nil
		} else if time.Now().Before(f.nextRetry) {
			m.mu.Unlock()
			m.logger.Info("%s %s start deferred (backoff, retry in %v)", m.adapter.PluginKey(), path, time.Until(f.nextRetry).Round(time.Second))
			return nil
		}
	}
	m.mu.Unlock()

	err := m.start(path, hash)
	if err != nil {
		m.mu.Lock()
		f = m.failures[path] // re-fetch: another goroutine may have updated this key between the two lock acquisitions
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
		m.logger.ErrorF("%s %s start failed (attempt %d), next retry in %v", m.adapter.PluginKey(), path, f.count, backoff)
		return err
	}

	// Success - clear any failure state.
	m.mu.Lock()
	delete(m.failures, path)
	m.mu.Unlock()
	return nil
}

// start spawns n workers and notifies the pool to register them.
func (m *PluginExecutor[T]) start(path, hash string) error {
	n := m.adapter.Workers(path)
	wrapped, handles, err := m.spawnN(path, hash, n)
	if err != nil {
		return err
	}
	m.notify(NewRegisterMessage[T](wrapped, len(handles)))
	return nil
}

// update spawns new workers and notifies the pool with an onDrained callback; the old subprocesses are killed only after in-flight calls on the old pool drain, so none hits a dead conn.
func (m *PluginExecutor[T]) update(path string, oldHandles []*PluginHandle, newHash string) error {
	n := m.adapter.Workers(path)
	wrapped, newHandles, err := m.spawnN(path, newHash, n)
	if err != nil {
		return err
	}
	onDrained := func() {
		for _, h := range oldHandles {
			m.kill(h)
		}
	}
	m.notify(NewUpdateMessage[T](wrapped, len(newHandles), onDrained))
	m.metrics.Updates.Inc()
	m.logger.Info("%s updated: %s (%d worker(s))", m.adapter.PluginKey(), path, len(newHandles))
	return nil
}

// kill gracefully shuts down the subprocess exactly once (concurrency-safe); it does NOT touch plugin_handles - callers that own the map entry call evict.
func (m *PluginExecutor[T]) kill(handle *PluginHandle) {
	handle.killOnce.Do(func() {
		close(handle.stopped)
		defer func() {
			if r := recover(); r != nil {
				m.logger.ErrorF("panic during shutdown of %s [%s]: %v", handle.Name, handle.Key.Id, r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = handle.Lifecycle.Shutdown(ctx)
		cancel()
		handle.Client.Kill()
		m.metrics.ActiveSubprocesses.WithLabelValues(m.adapter.PluginKey()).Dec()
	})
}

// evict kills all handles and deletes the group from plugin_handles (write lock only for the delete, so kill() runs outside it).
// Guards a concurrent replacement at the same key by checking the stored slice still begins with the same pointer.
func (m *PluginExecutor[T]) evict(key string, handles []*PluginHandle) {
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

// stop evicts transiently (crash-restart or config-disable) and sends UnregisterMessage - pool drops the entry but does NOT tombstone.
func (m *PluginExecutor[T]) stop(key string, handles []*PluginHandle) {
	m.evict(key, handles)
	m.notify(NewUnregisterMessage[T](handles[0].Key))
	m.logger.Info("%s stopped: %s [%s]", m.adapter.PluginKey(), handles[0].Name, handles[0].Key.Id)
}

// remove evicts permanently (binary deleted from disk) and sends RemoveMessage - pool drops the entry AND tombstones the plugin ID.
func (m *PluginExecutor[T]) remove(key string, handles []*PluginHandle) {
	m.evict(key, handles)
	m.notify(NewRemoveMessage[T](handles[0].Key))
	m.logger.Info("%s removed: %s [%s]", m.adapter.PluginKey(), handles[0].Name, handles[0].Key.Id)
}

// restart stops then re-starts (with backoff), setting restarting[path] first so reconcile doesn't race to refill the empty slot;
// bails if restarting[path] is already set (another pingLoop worker beat us to it).
func (m *PluginExecutor[T]) restart(key string, handles []*PluginHandle) error {
	path := handles[0].BinPath
	hash := handles[0].Key.Hash

	m.mu.Lock()
	if _, already := m.restarting[path]; already {
		m.mu.Unlock()
		return nil // another pingLoop worker is already handling this restart
	}
	m.restarting[path] = struct{}{}
	m.mu.Unlock()

	m.stop(key, handles)

	err := m.startWithBackoff(path, hash)

	m.mu.Lock()
	delete(m.restarting, path)
	m.mu.Unlock()

	return err
}

func (m *PluginExecutor[T]) pingLoop(handle *PluginHandle) {
	interval := m.pingInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-handle.stopped:
			return // intentionally stopped - do not restart
		case <-t.C:
			// During a graceful update spawnN stores new handles before notify; if this handle is no longer in the slice it was replaced - exit without restarting.
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
				m.logger.ErrorF("%s crash/health fail %s: %v - restarting", m.adapter.PluginKey(), handle.Name, err)
				// Fetch the full current group so restart kills all workers, not just this one.
				m.mu.RLock()
				group := m.plugin_handles[handle.BinPath]
				m.mu.RUnlock()
				// Guard: if our handle is no longer in the group, update() replaced it mid-Ping; the new workers' pingLoops own future restarts.
				inGroup := false
				for _, h := range group {
					if h == handle {
						inGroup = true
						break
					}
				}
				if !inGroup {
					return
				}
				if restartErr := m.restart(handle.BinPath, group); restartErr != nil {
					m.logger.Error(errors.NewF("restart failed for %s: %v", handle.BinPath, restartErr))
				}
				m.metrics.Restarts.Inc()
				return
			}
		}
	}
}
