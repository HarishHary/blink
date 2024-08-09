package scoring

type SignalType = string

var SignalTypeEnum = struct {
	Core SignalType
	Leaf SignalType
}{
	Core: "core",
	Leaf: "leaf",
}

// Function to compute the signal type
func ComputeSignalType(confidence Confidence) SignalType {
	if confidence >= ConfidenceEnum.Medium {
		return SignalTypeEnum.Core
	}
	return SignalTypeEnum.Leaf
}
