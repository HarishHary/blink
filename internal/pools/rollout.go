package pools

import "hash/fnv"

// MissingTenantRolloutKey is the reserved rollout key for events whose
// tenant_id is missing, empty, or not a string. It must remain stable and non-empty.
const MissingTenantRolloutKey = "\x00missing-tenant\x00"

// NormalizeRolloutKey returns the canonical rollout key for tenantID.
// Existing non-empty tenant IDs remain unchanged to preserve their rollout buckets.
func NormalizeRolloutKey(tenantID any) string {
	if id, ok := tenantID.(string); ok && id != "" {
		return id
	}
	return MissingTenantRolloutKey
}

// RolloutBucket returns rolloutKey's stable bucket in the range 1-100.
// Empty keys use the same bucket as a missing tenant.
func RolloutBucket(rolloutKey string) uint32 {
	if rolloutKey == "" {
		rolloutKey = MissingTenantRolloutKey
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(rolloutKey))
	return h.Sum32()%100 + 1
}
