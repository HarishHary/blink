package snapshot

import (
	"encoding/binary"
	"fmt"

	"github.com/harishhary/blink/internal/pools"
	"github.com/harishhary/blink/internal/snapshot/pb"
)

// GenerationMarkerKey is the reserved key the controller uses on the keyed snapshot topic to
// publish the current DB generation for fleet rollout tracking. It is not a logical plugin ID:
// readers special-case it (updating AppliedGeneration) and never treat it as an EffectiveEntry.
// The name is chosen so it cannot collide with a plugin ID.
const GenerationMarkerKey = "__blink_generation__"

// EncodeGeneration marshals a generation-marker value as a big-endian int64.
func EncodeGeneration(gen int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(gen))
	return b
}

// DecodeGeneration parses a generation-marker value written by EncodeGeneration.
func DecodeGeneration(b []byte) (int64, error) {
	if len(b) != 8 {
		return 0, fmt.Errorf("generation marker: want 8 bytes, got %d", len(b))
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

// EntryToProto converts one EffectiveEntry to its protobuf form. The per-ID keyed snapshot
// topic publishes these individually (one message per logical plugin ID).
func EntryToProto(e EffectiveEntry) *pb.EffectiveEntry {
	return &pb.EffectiveEntry{
		Id:        e.Id,
		Enabled:   e.Enabled,
		Primary:   refToProto(e.Primary),
		Candidate: refToProto(e.Candidate),
	}
}

// EntryFromProto converts one protobuf EffectiveEntry back to the domain type.
func EntryFromProto(e *pb.EffectiveEntry) EffectiveEntry {
	return EffectiveEntry{
		Id:        e.GetId(),
		Enabled:   e.GetEnabled(),
		Primary:   refFromProto(e.GetPrimary()),
		Candidate: refFromProto(e.GetCandidate()),
	}
}

func refToProto(r *ArtifactRef) *pb.ArtifactRef {
	if r == nil {
		return nil
	}
	return &pb.ArtifactRef{Name: r.Name, Mode: pb.RolloutMode(r.Mode), Spec: r.Spec, Hash: r.Hash}
}

func refFromProto(r *pb.ArtifactRef) *ArtifactRef {
	if r == nil {
		return nil
	}
	return &ArtifactRef{Name: r.GetName(), Mode: pools.RolloutMode(r.GetMode()), Spec: r.GetSpec(), Hash: r.GetHash()}
}
