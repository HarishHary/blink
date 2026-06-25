package plugin

import (
	"context"

	"github.com/harishhary/blink/internal/helpers"
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

// Workers returns how many subprocess instances to spawn for this binary.
func (a *PluginAdapter[T]) Workers(binPath string) int {
	d, ok := a.DesiredConfig.DesiredBinaryState(helpers.BinaryBaseName(binPath))
	if !ok || d.MaxProcs <= 0 {
		return 1
	}
	return d.MaxProcs
}
