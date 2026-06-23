package executor

import internal "github.com/harishhary/blink/internal/pools"

// BinaryState is the normalized desired state for a single binary,
// derived from its YAML sidecar. PluginAdapter uses this to implement the
// shared adapter methods without per-type config manager dependencies.
type BinaryState struct {
	ID       string
	Name     string
	Enabled  bool
	Mode     internal.RolloutMode
	MaxProcs int
}

// DesiredConfig is the read-only interface PluginAdapter uses to query the
// desired config of a binary. It is satisfied directly by the type-specific
// config manager and passed to NewXxxAdapter.
type DesiredConfig interface {
	// DesiredBinaryState returns the desired state keyed by binary filename (no extension).
	// Returns false when no YAML sidecar exists for the binary.
	DesiredBinaryState(name string) (BinaryState, bool)
	// HasBlockingErrorFor reports whether the plugin has a validation error that
	// prevents it from starting. Plugin types without validation should return false.
	HasBlockingErrorFor(pluginID string, yamlFile string) bool
}
