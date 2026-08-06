package snapshot

import (
	"encoding/binary"
	"fmt"

	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/snapshot/pb"
	"google.golang.org/protobuf/proto"
)

// Marshal serialises one EffectiveEntry to protobuf bytes for the per-ID keyed snapshot topic.
func Marshal(e EffectiveEntry) ([]byte, error) {
	return proto.Marshal(EntryToProto(e))
}

// Unmarshal deserialises a snapshot-topic value back into an EffectiveEntry.
func Unmarshal(data []byte) (EffectiveEntry, error) {
	var pe pb.EffectiveEntry
	if err := proto.Unmarshal(data, &pe); err != nil {
		return EffectiveEntry{}, err
	}
	return ProtoToEntry(&pe), nil
}

// EntryToProto converts one EffectiveEntry to its protobuf form.
func EntryToProto(e EffectiveEntry) *pb.EffectiveEntry {
	return &pb.EffectiveEntry{
		Id:        e.Id,
		Enabled:   e.Enabled,
		Primary:   refToProto(e.Primary),
		Candidate: refToProto(e.Candidate),
	}
}

// ProtoToEntry converts one protobuf EffectiveEntry back to the domain type.
func ProtoToEntry(e *pb.EffectiveEntry) EffectiveEntry {
	return EffectiveEntry{
		Id:        e.GetId(),
		Enabled:   e.GetEnabled(),
		Primary:   protoToRef(e.GetPrimary()),
		Candidate: protoToRef(e.GetCandidate()),
	}
}

// refToProto converts an ArtifactRef to its protobuf form (nil-safe).
func refToProto(r *ArtifactRef) *pb.ArtifactRef {
	if r == nil {
		return nil
	}
	return &pb.ArtifactRef{Name: r.Name, Mode: pb.RolloutMode(r.RolloutMode), Spec: r.Spec, Hash: r.Hash}
}

// protoToRef converts a protobuf ArtifactRef back to the domain type (nil-safe).
func protoToRef(r *pb.ArtifactRef) *ArtifactRef {
	if r == nil {
		return nil
	}
	return &ArtifactRef{
		Name:        r.GetName(),
		RolloutMode: runtime.RolloutMode(r.GetMode()),
		Spec:        r.GetSpec(),
		Hash:        r.GetHash(),
	}
}

// --- Generation marker ---
// A distinct codec from the entry codec above: the controller publishes the current DB
// generation under a reserved key (not protobuf, just a big-endian int64) for rollout tracking.

// GenerationMarkerKey is the reserved key the controller uses on the keyed snapshot topic to
// publish the current DB generation for fleet rollout tracking. It is not a logical plugin ID:
// readers special-case it (updating AppliedGeneration) and never treat it as an EffectiveEntry.
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
