package events

// wireSizeSampleCount bounds how many events SampleWireSize walks; the stride still spans them.
const wireSizeSampleCount = 64

// WireSize estimates the bytes one event costs inside a plugin request: its protobuf struct
// encoding as one element of a repeated field. An estimate, so MaxCallPayloadBytes keeps headroom.
func (e Event) WireSize() int {
	return RepeatedFieldSize(structWireSize(e))
}

// RepeatedFieldSize is what a body of the given size costs as one element of a repeated field: the
// body plus its framing. Exported for the alert path, which prices bodies with proto.Size.
func RepeatedFieldSize(body int) int {
	return 1 + varintWireSize(body) + body
}

// SampleWireSize estimates the largest event in the batch from a bounded sample, zero for an empty
// one. The largest, not the average: an underestimate costs a call that fails outright.
func SampleWireSize(in []Event) int {
	if len(in) == 0 {
		return 0
	}
	stride := max(1, len(in)/wireSizeSampleCount)
	largest := 0
	for i := 0; i < len(in); i += stride {
		if size := in[i].WireSize(); size > largest {
			largest = size
		}
	}
	return largest
}

// structWireSize is the encoded size of a structpb.Struct body, one framed entry per field.
func structWireSize(fields map[string]any) int {
	total := 0
	for key, value := range fields {
		body := valueWireSize(value)
		entry := 1 + varintWireSize(len(key)) + len(key) + 1 + varintWireSize(body) + body
		total += 1 + varintWireSize(entry) + entry
	}
	return total
}

// valueWireSize is the encoded size of a structpb.Value body, the oneof field the Go value maps
// onto. Anything structpb cannot represent is charged a constant; the conversion rejects it later.
func valueWireSize(value any) int {
	switch value := value.(type) {
	case nil:
		return 2 // null_value: tag plus a single-byte enum
	case bool:
		return 2
	case string:
		return 1 + varintWireSize(len(value)) + len(value)
	case Event:
		body := structWireSize(value)
		return 1 + varintWireSize(body) + body
	case map[string]any:
		body := structWireSize(value)
		return 1 + varintWireSize(body) + body
	case []any:
		body := 0
		for _, element := range value {
			size := valueWireSize(element)
			body += 1 + varintWireSize(size) + size
		}
		return 1 + varintWireSize(body) + body
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return 9 // number_value: tag plus a fixed64 double
	default:
		return 9
	}
}

// varintWireSize is how many bytes a protobuf varint of n takes, which every tag and length costs.
func varintWireSize(n int) int {
	switch {
	case n < 1<<7:
		return 1
	case n < 1<<14:
		return 2
	case n < 1<<21:
		return 3
	case n < 1<<28:
		return 4
	default:
		return 5
	}
}
