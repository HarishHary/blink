package dlq

import (
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/dlq/pb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Record wraps a failed input in a DLQEnvelope addressed to its source offset. stage names the
// pipeline step that gave up, reason is the operator-facing detail, and attempts is how many
// tries were burned (0 when the input was rejected without ever being processed).
func Record(source brokers.Message, stage, reason string, attempts int) (brokers.Message, error) {
	payload, err := proto.Marshal(&pb.DLQEnvelope{
		Source: &pb.DLQSource{
			Topic:     source.Topic,
			Partition: int32(source.Partition),
			Offset:    source.Offset,
		},
		OriginalPayload: source.Value,
		Stage:           stage,
		Reason:          reason,
		Attempts:        int32(attempts),
		FailedAt:        timestamppb.Now(),
	})
	if err != nil {
		return brokers.Message{}, err
	}
	return brokers.Message{Key: append([]byte(nil), source.Key...), Value: payload}, nil
}
