package plugin

import (
	"context"

	"github.com/harishhary/blink/internal/helpers"
	goplugin "github.com/hashicorp/go-plugin"
)

// PluginAdapter[T] encapsulates the type-specific plugin logic; the shared methods (IsReady, Workers)
// delegate to Config (a DesiredConfig), and only DoHandshake is type-specific. Built per type via NewXxxAdapter.
type PluginAdapter[T Syncable] struct {
	Key         string
	Magic       string
	Plugin      goplugin.Plugin
	Config      DesiredConfig
	DoHandshake func(ctx context.Context, raw any, binPath, hash string) (T, PluginRPC, error)
}

func (a *PluginAdapter[T]) PluginKey() string           { return a.Key }
func (a *PluginAdapter[T]) MagicValue() string          { return a.Magic }
func (a *PluginAdapter[T]) GRPCPlugin() goplugin.Plugin { return a.Plugin }
func (a *PluginAdapter[T]) Handshake(ctx context.Context, raw interface{}, binPath string, hash string) (T, PluginRPC, error) {
	return a.DoHandshake(ctx, raw, binPath, hash)
}

// IsReady reports whether the snapshot named a parseable spec for this binary.
// False makes reconcile skip the binary silently (no error, no backoff) until the next snapshot reconcile.
func (a *PluginAdapter[T]) IsReady(binPath string) bool {
	_, ok := a.Config.DesiredBinaryState(helpers.BinaryBaseName(binPath))
	return ok
}

// Workers returns how many subprocess instances to spawn for this binary.
func (a *PluginAdapter[T]) Workers(binPath string) int {
	d, ok := a.Config.DesiredBinaryState(helpers.BinaryBaseName(binPath))
	if !ok || d.MaxProcs <= 0 {
		return 1
	}
	return d.MaxProcs
}
