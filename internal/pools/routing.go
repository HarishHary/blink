package pools

import "fmt"

// RolloutMode controls how traffic is split between old and new plugin versions.
type RolloutMode int

const (
	// RolloutModeBlueGreen (default): pre-warm new pool, flip generation, drain old.
	RolloutModeBlueGreen RolloutMode = iota
	// RolloutModeCanary: route RolloutPct% of calls (by consistent hash) to the new version.
	RolloutModeCanary
	// RolloutModeShadow: call new version in background; discard result; log errors.
	RolloutModeShadow
)

// String returns the mode's canonical name: "blue-green", "canary", or "shadow".
func (m RolloutMode) String() string {
	switch m {
	case RolloutModeBlueGreen:
		return "blue-green"
	case RolloutModeCanary:
		return "canary"
	case RolloutModeShadow:
		return "shadow"
	default:
		return fmt.Sprintf("RolloutMode(%d)", int(m))
	}
}

// MarshalText encodes the mode as its String() name, for YAML/JSON round-tripping.
func (m RolloutMode) MarshalText() ([]byte, error) {
	return []byte(m.String()), nil
}

// UnmarshalText parses a mode name; empty and "bluegreen" both map to blue-green.
func (m *RolloutMode) UnmarshalText(b []byte) error {
	switch string(b) {
	case "blue-green", "bluegreen", "":
		*m = RolloutModeBlueGreen
	case "canary":
		*m = RolloutModeCanary
	case "shadow":
		*m = RolloutModeShadow
	default:
		return fmt.Errorf("unknown rollout mode %q", string(b))
	}
	return nil
}

// RolloutConfig returns per-plugin (mode, rolloutPct): name != "" at Register (per-binary lookup),
// name == "" at Call (merged id-level); zero values mean blue-green.
type RolloutConfig func(pluginID, name string) (mode RolloutMode, rolloutPct float64)

// RolloutEntry holds the rollout configuration for a single plugin.
type RolloutEntry struct {
	RolloutMode RolloutMode
	RolloutPct  float64
}
