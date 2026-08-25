package alerts

import (
	"github.com/harishhary/blink/pkg/events"
)

// Batch is a set of alerts paired with their encodings: encoding an alert converts a pb.Alert that embeds its rule's
// whole metadata, so an alert evaluated by five tuning rules used to pay for that five times over.
type Batch = events.EncodedBatch[*Alert]

// NewBatch pairs alerts with their encodings, one per alert and in the same order.
func NewBatch(in []*Alert, raw [][]byte) *Batch { return events.NewEncodedBatch(in, raw) }

// PrepareBatch encodes every alert once; a caller that can dead-letter a single alert encodes where it decodes instead.
func PrepareBatch(in []*Alert) (*Batch, error) { return events.PrepareEncodedBatch(in, Marshal) }

