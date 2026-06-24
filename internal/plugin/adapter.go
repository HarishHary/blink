package plugin

import (
	"context"

	"github.com/harishhary/blink/internal/helpers"
	internal "github.com/harishhary/blink/internal/pools"
	goplugin "github.com/hashicorp/go-plugin"
)

// PluginAdapter[T] encapsulates every piece of type-specific plugin logic.
// Create one per plugin type with the pkg-level NewXxxAdapter constructor and
// inject into NewPluginExecutor. Shared methods (IsReady, CurrentMode, IsShadow,
// IsEnabled, Workers) are implemented here by delegating to DesiredConfig; only
// DoHandshake is type-specific.
type PluginAdapter[T Syncable] struct {
	Key           string
	Magic         string
	Plugin        goplugin.Plugin
	DesiredConfig DesiredConfig
	DoHandshake   func(ctx context.Context, raw any, binPath, hash string) (T, PluginLifecycle, string, string, error)
}

func (a *PluginAdapter[T]) PluginKey() string { return a.Key }

func (a *PluginAdapter[T]) MagicValue() string { return a.Magic }

func (a *PluginAdapter[T]) GRPCPlugin() goplugin.Plugin { return a.Plugin }

func (a *PluginAdapter[T]) Handshake(ctx context.Context, raw interface{}, binPath string, hash string) (T, PluginLifecycle, string, string, error) {
	return a.DoHandshake(ctx, raw, binPath, hash)
}

// IsReady reports whether all external prerequisites for this binary exist (e.g. a
// required sidecar config file). Returning false causes reconcile to skip the binary
// silently — no error, no backoff — until the next reconcile (fsnotify event or 5s poll).
func (a *PluginAdapter[T]) IsReady(binPath string) bool {
	name := helpers.BinaryBaseName(binPath)
	d, ok := a.DesiredConfig.DesiredBinaryState(name)
	if !ok {
		return false
	}
	return !a.DesiredConfig.HasBlockingErrorFor(d.ID, name+".yaml")
}

// CurrentMode returns the rollout mode declared in the binary's current YAML sidecar.
// reconcile() compares this against PluginHandle.Mode to detect YAML-only mode changes.
func (a *PluginAdapter[T]) CurrentMode(binPath string) internal.RolloutMode {
	d, ok := a.DesiredConfig.DesiredBinaryState(helpers.BinaryBaseName(binPath))
	if !ok {
		return internal.RolloutModeBlueGreen
	}
	return d.Mode
}

// IsShadow reports whether this binary is a canary/shadow version that should NOT
// claim the active pool slot on a fresh start.
func (a *PluginAdapter[T]) IsShadow(binPath string) bool {
	mode := a.CurrentMode(binPath)
	return mode == internal.RolloutModeCanary || mode == internal.RolloutModeShadow
}

// IsEnabled reports whether a running handle should continue running.
func (a *PluginAdapter[T]) IsEnabled(h *PluginHandle) bool {
	d, ok := a.DesiredConfig.DesiredBinaryState(helpers.BinaryBaseName(h.BinPath))
	return ok && d.Enabled
}

// Workers returns how many subprocess instances to spawn for this binary.
func (a *PluginAdapter[T]) Workers(binPath string) int {
	d, ok := a.DesiredConfig.DesiredBinaryState(helpers.BinaryBaseName(binPath))
	if !ok || d.MaxProcs <= 0 {
		return 1
	}
	return d.MaxProcs
}
