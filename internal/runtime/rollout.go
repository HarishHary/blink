package runtime

import (
	"fmt"
	"hash/fnv"
)

type RolloutMode int

const (
	RolloutModeBlueGreen RolloutMode = iota
	RolloutModeCanary
	RolloutModeShadow
)

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

func (m RolloutMode) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

func (m *RolloutMode) UnmarshalText(text []byte) error {
	switch string(text) {
	case "blue-green", "bluegreen", "":
		*m = RolloutModeBlueGreen
	case "canary":
		*m = RolloutModeCanary
	case "shadow":
		*m = RolloutModeShadow
	default:
		return fmt.Errorf("unknown rollout mode %q", string(text))
	}
	return nil
}

type RolloutEntry struct {
	RolloutMode RolloutMode
	RolloutPct  float64
}

type PoolKey struct {
	Id   string
	Name string
	Hash string
}

func (k PoolKey) String() string {
	if k.Hash != "" {
		return k.Id + "@" + k.Name + "@" + k.Hash
	}
	return k.Id + "@" + k.Name
}

const MissingTenantRolloutKey = "\x00missing-tenant\x00"

func NormalizeRolloutKey(value any) string {
	if key, ok := value.(string); ok && key != "" {
		return key
	}
	return MissingTenantRolloutKey
}

func RolloutBucket(key string) uint32 {
	if key == "" {
		key = MissingTenantRolloutKey
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return h.Sum32()%100 + 1
}
