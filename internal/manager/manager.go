// Package manager defines the Manager interface implemented by both PluginExecutor and ConfigManager - anything that can be started with a context.
package manager

import "context"

type Manager interface {
	Start(ctx context.Context) error
}
