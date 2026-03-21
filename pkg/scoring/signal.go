package scoring

import (
	"encoding/json"
	"fmt"
)

type SignalType int

const (
	SignalTypeLeaf SignalType = iota // 0
	SignalTypeCore                   // 1
)

var SignalTypeEnum = struct {
	Core, Leaf SignalType
}{
	Core: SignalTypeCore,
	Leaf: SignalTypeLeaf,
}

func (s SignalType) String() string {
	switch s {
	case SignalTypeCore:
		return "core"
	case SignalTypeLeaf:
		return "leaf"
	default:
		return fmt.Sprintf("signaltype(%d)", int(s))
	}
}

func (s SignalType) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

func (s *SignalType) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	switch str {
	case "core":
		*s = SignalTypeCore
	case "leaf":
		*s = SignalTypeLeaf
	default:
		return fmt.Errorf("unknown signal type %q", str)
	}
	return nil
}

func ComputeSignalType(confidence Confidence) SignalType {
	if confidence >= ConfidenceMedium {
		return SignalTypeCore
	}
	return SignalTypeLeaf
}
