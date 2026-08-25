package events

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// Marshal encodes the event as a protobuf struct, the form every plugin request and the executor record carry it in.
func (e Event) Marshal() ([]byte, error) {
	value, err := structpb.NewStruct(e)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(value)
}

// Batchable is what a batched item has to be: able to hand out a copy the caller owns, and able to name the side it routes to.
type Batchable[T any] interface {
	Clone() T
	RolloutKey() string
}

// EncodedBatch is items paired with their encodings, prepared once and shared by every caller that sends them.
// The encodings are immutable, so a batch is safe to hand to concurrent calls and to calls that outlive the caller.
type EncodedBatch[T Batchable[T]] struct {
	items []T
	raw   [][]byte
	sizes []int
	keys  []string
}

// NewEncodedBatch pairs items with their encodings, one per item in the same order, and prices and keys them while it is here.
func NewEncodedBatch[T Batchable[T]](in []T, raw [][]byte) *EncodedBatch[T] {
	sizes := make([]int, len(raw))
	keys := make([]string, len(in))
	for i, encoded := range raw {
		sizes[i] = repeatedFieldSize(len(encoded))
	}
	for i, item := range in {
		keys[i] = item.RolloutKey()
	}
	return &EncodedBatch[T]{items: in, raw: raw, sizes: sizes, keys: keys}
}

// PrepareEncodedBatch encodes every item once, and one item that will not encode fails the whole batch.
func PrepareEncodedBatch[T Batchable[T]](in []T, marshal func(T) ([]byte, error)) (*EncodedBatch[T], error) {
	raw := make([][]byte, len(in))
	for i, item := range in {
		encoded, err := marshal(item)
		if err != nil {
			return nil, err
		}
		raw[i] = encoded
	}
	return NewEncodedBatch(in, raw), nil
}

func (b *EncodedBatch[T]) Len() int {
	if b == nil {
		return 0
	}
	return len(b.raw)
}

// Raw is the per-item encodings, in batch order.
func (b *EncodedBatch[T]) Raw() [][]byte {
	if b == nil {
		return nil
	}
	return b.raw
}

// At is the item at i without copying it, for a caller reading its own fields - a routing key, an id.
func (b *EncodedBatch[T]) At(i int) T { return b.items[i] }

// Items is the batch's items deep-copied, for a caller handing them to code that may mutate what it receives.
func (b *EncodedBatch[T]) Items() []T {
	if b == nil {
		return nil
	}
	out := make([]T, len(b.items))
	for i, item := range b.items {
		out[i] = item.Clone()
	}
	return out
}

// Clone returns a batch over independently owned items, sharing the encodings, so only the caller decides what to keep.
func (b *EncodedBatch[T]) Clone() *EncodedBatch[T] {
	if b == nil {
		return nil
	}
	return &EncodedBatch[T]{items: b.Items(), raw: b.raw, sizes: b.sizes, keys: b.keys}
}

// Gather picks the positions listed, sharing their items, encodings, and prices.
func (b *EncodedBatch[T]) Gather(indexes []int) *EncodedBatch[T] {
	out := &EncodedBatch[T]{items: make([]T, len(indexes)), raw: make([][]byte, len(indexes)), sizes: make([]int, len(indexes)), keys: make([]string, len(indexes))}
	for i, index := range indexes {
		out.items[i] = b.items[index]
		out.raw[i] = b.raw[index]
		out.sizes[i] = b.sizes[index]
		out.keys[i] = b.keys[index]
	}
	return out
}

// Slice is the half-open range of the batch, sharing its items, encodings, and prices.
func (b *EncodedBatch[T]) Slice(start, end int) *EncodedBatch[T] {
	return &EncodedBatch[T]{items: b.items[start:end], raw: b.raw[start:end], sizes: b.sizes[start:end], keys: b.keys[start:end]}
}

// WireSizes is what each item costs inside a request: its encoding plus the repeated-field framing, exact and priced once.
func (b *EncodedBatch[T]) WireSizes() []int {
	if b == nil {
		return nil
	}
	return b.sizes
}

// RolloutKeys is each item's rollout key in batch order, keyed once with the batch because every plugin routing it needs the same keys.
func (b *EncodedBatch[T]) RolloutKeys() []string {
	if b == nil {
		return nil
	}
	return b.keys
}

// Batch is a set of events paired with their encodings: one encoding serves every caller, and a batch fanned out to
// fifty matchers used to convert and marshal each event fifty times over.
type Batch = EncodedBatch[Event]

// NewBatch pairs events with their encodings, one per event and in the same order.
func NewBatch(in []Event, raw [][]byte) *Batch { return NewEncodedBatch(in, raw) }

// PrepareBatch encodes every event once; a caller already holding an encoding per event pairs them with NewBatch instead.
func PrepareBatch(in []Event) (*Batch, error) { return PrepareEncodedBatch(in, Event.Marshal) }

// repeatedFieldSize is what a body of the given size costs as one repeated-field element: the body, a tag byte, and a length varint.
func repeatedFieldSize(body int) int {
	switch {
	case body < 1<<7:
		return 2 + body
	case body < 1<<14:
		return 3 + body
	case body < 1<<21:
		return 4 + body
	case body < 1<<28:
		return 5 + body
	default:
		return 6 + body
	}
}
