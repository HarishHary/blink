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

// ArtifactKey identifies one artifact incarnation of a plugin: the plugin's id and name together
// with the content hash that tells one build of it from the next.
type ArtifactKey struct {
	Id   string
	Name string
	Hash string
}

// String renders the key for logs and errors, and is what formatting any key that embeds this one
// prints.
func (k ArtifactKey) String() string {
	if k.Hash != "" {
		return k.Id + "@" + k.Name + "@" + k.Hash
	}
	return k.Id + "@" + k.Name
}

const MissingTenantRolloutKey = "\x00missing-tenant\x00"

// RolloutBucketCount is how many buckets RolloutBucket hashes into, and so the largest
// number of rollout groups any batch can be split into however many events it holds.
const RolloutBucketCount = 100

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
	return h.Sum32()%RolloutBucketCount + 1
}
