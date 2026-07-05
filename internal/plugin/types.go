package plugin

import (
	"context"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/pools"
	goplugin "github.com/hashicorp/go-plugin"
)

// PluginMetadata holds the common identity and rollout fields shared by all plugin types.
// YAML tags are present so config packages can embed this struct with yaml:",inline".
// Name is NOT read from YAML - it is derived from the filename at load time.
type PluginMetadata struct {
	Id          string            `yaml:"id"`
	Name        string            `yaml:"-"`
	DisplayName string            `yaml:"display_name"`
	Description string            `yaml:"description"`
	Enabled     bool              `yaml:"enabled"`
	Version     string            `yaml:"version"`
	RolloutMode pools.RolloutMode `yaml:"mode"` // UnmarshalText handles "blue-green" / "canary" / "shadow"
	RolloutPct  float64           `yaml:"rollout_pct"`
	MinProcs    int               `yaml:"min_procs"`
	MaxProcs    int               `yaml:"max_procs"`
}

func (m PluginMetadata) Metadata() PluginMetadata { return m }
func (m *PluginMetadata) SetName(name string)     { m.Name = name }

// Checksum satisfies Syncable for the *Metadata config types, which have no binary checksum; runtime
// plugin handles override it with the loaded binary's hash.
func (m PluginMetadata) Checksum() string { return "" }

// Syncable is the type constraint for all plugin types managed by a Manager. Checksum distinguishes
// binary versions: runtime handles return the loaded binary's hash, the *Metadata config types "".
type Syncable interface {
	Metadata() PluginMetadata
	Checksum() string
}

// PluginLifecycle provides the health-check and graceful-shutdown primitives the Manager uses in ping loops and kill paths.
type PluginLifecycle interface {
	Ping(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// PluginHandle tracks everything the Manager needs for one running plugin subprocess.
type PluginHandle struct {
	Client    *goplugin.Client
	Lifecycle PluginLifecycle
	BinPath   string
	Key       pools.PoolKey     // {Id, Name, Hash}; Id used for logging, Key used for bus messages
	Mode      pools.RolloutMode // mode at spawn time; compared against live YAML to detect soft-restart triggers
	Name      string            // human-readable display name; used for logging
	killOnce  sync.Once
	stopped   chan struct{}
}

// startFailure tracks consecutive start failures for a binary path.
type startFailure struct {
	count     int
	nextRetry time.Time
	hash      string // hash at time of last failure; reset backoff if binary changes
}
