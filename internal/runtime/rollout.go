package runtime

import (
	"fmt"
	"hash/fnv"
)

// RolloutMode is how a candidate artifact takes traffic beside the primary.
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

// ArtifactKey identifies one artifact incarnation of a plugin: its id, its name, and its content hash.
type ArtifactKey struct {
	Id   string
	Name string
	Hash string
}

// String renders the key for logs and errors, and for any key that embeds this one.
func (k ArtifactKey) String() string {
	if k.Hash != "" {
		return k.Id + "@" + k.Name + "@" + k.Hash
	}
	return k.Id + "@" + k.Name
}

// MissingTenantRolloutKey stands in for an item carrying no usable tenant, so it still routes somewhere.
const MissingTenantRolloutKey = "\x00missing-tenant\x00"

// RolloutBucketCount is how many buckets RolloutBucket hashes into, and so the most groups any batch can split into.
const RolloutBucketCount = 100

// NormalizeRolloutKey turns a raw field value into a rollout key, falling back for anything unusable.
func NormalizeRolloutKey(value any) string {
	if key, ok := value.(string); ok && key != "" {
		return key
	}
	return MissingTenantRolloutKey
}

// RolloutBucket hashes a rollout key into 1..RolloutBucketCount, which is what a canary percentage compares against.
func RolloutBucket(key string) uint32 {
	if key == "" {
		key = MissingTenantRolloutKey
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return h.Sum32()%RolloutBucketCount + 1
}

// RouteGroup is the positions routing to one side, named by the first key seen there since all of them route alike.
type RouteGroup struct {
	Key     string
	Indexes []int
}

// RouteSides groups a batch's positions by the side a canary at this percentage takes them to, nil when one side takes all.
func RouteSides(keys []string, canaryPct float64) []RouteGroup {
	// Only a partial canary splits a batch: with no candidate, or a whole one that is elected primary, every key routes alike.
	if canaryPct <= 0 || canaryPct >= RolloutBucketCount {
		return nil
	}
	groups := make([]RouteGroup, 0, 2)
	groupBySide := [2]int{-1, -1}
	for i, key := range keys {
		side := 0
		if float64(RolloutBucket(key)) <= canaryPct {
			side = 1
		}
		if groupBySide[side] < 0 {
			groupBySide[side] = len(groups)
			groups = append(groups, RouteGroup{Key: key})
		}
		group := &groups[groupBySide[side]]
		group.Indexes = append(group.Indexes, i)
	}
	if len(groups) == 1 {
		return nil
	}
	return groups
}
