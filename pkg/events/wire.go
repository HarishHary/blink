package events

// RepeatedFieldSize is what a body of the given size costs as one element of a repeated field: the
// body plus its framing. Exported for the alert path, which prices bodies with proto.Size.
func RepeatedFieldSize(body int) int {
	return 1 + varintWireSize(body) + body
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
