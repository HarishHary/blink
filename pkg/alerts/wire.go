package alerts

import (
	"google.golang.org/protobuf/proto"

	"github.com/harishhary/blink/pkg/events"
)

// wireSizeSampleCount is smaller than the event path's because pricing an alert means converting
// it: pb.Alert embeds its rule's whole metadata, so no arithmetic model of it would stay true.
const wireSizeSampleCount = 8

// SampleWireSize estimates the bytes the largest alert in a batch costs inside a plugin request, as
// a framed element of a repeated pb.Alert field. The largest, not the average: an underestimate
// costs a call that fails outright. An alert that will not convert is skipped, since the call is
// about to fail the same conversion with a better error; zero means none of the sample converted.
func SampleWireSize(in []*Alert) int {
	if len(in) == 0 {
		return 0
	}
	stride := max(1, len(in)/wireSizeSampleCount)
	largest := 0
	for i := 0; i < len(in); i += stride {
		p, err := AlertToProto(in[i])
		if err != nil {
			continue
		}
		if size := events.RepeatedFieldSize(proto.Size(p)); size > largest {
			largest = size
		}
	}
	return largest
}
