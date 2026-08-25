package alerts

import (
	"github.com/harishhary/blink/pkg/events"
)

// Batch is alerts paired with their encodings: an alert carries its whole rule, which five tuning rules used to pay for five times.
type Batch = events.EncodedBatch[*Alert]

// NewBatch pairs alerts with their encodings, one per alert and in the same order.
func NewBatch(in []*Alert, raw [][]byte) *Batch { return events.NewEncodedBatch(in, raw) }

// PrepareBatch encodes every alert once; a caller that can dead-letter a single alert encodes where it decodes instead.
func PrepareBatch(in []*Alert) (*Batch, error) { return events.PrepareEncodedBatch(in, Marshal) }
