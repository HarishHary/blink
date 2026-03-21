package scoring

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Confidence int

const (
	ConfidenceVeryLow Confidence = iota
	ConfidenceLow
	ConfidenceMedium
	ConfidenceHigh
	ConfidenceVeryHigh
)

var ConfidenceEnum = struct {
	VeryLow, Low, Medium, High, VeryHigh Confidence
}{
	VeryLow:  ConfidenceVeryLow,
	Low:      ConfidenceLow,
	Medium:   ConfidenceMedium,
	High:     ConfidenceHigh,
	VeryHigh: ConfidenceVeryHigh,
}

func (c Confidence) String() string {
	switch c {
	case ConfidenceVeryLow:
		return "verylow"
	case ConfidenceLow:
		return "low"
	case ConfidenceMedium:
		return "medium"
	case ConfidenceHigh:
		return "high"
	case ConfidenceVeryHigh:
		return "veryhigh"
	default:
		return fmt.Sprintf("confidence(%d)", int(c))
	}
}

func (c Confidence) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.String())
}

func (c *Confidence) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	v, err := ParseConfidence(str)
	if err != nil {
		return err
	}
	*c = v
	return nil
}

func IsValidConfidence(c Confidence) bool {
	return c >= ConfidenceVeryLow && c <= ConfidenceVeryHigh
}

func ParseConfidence(s string) (Confidence, error) {
	normalized := strings.ToLower(strings.ReplaceAll(s, "_", ""))
	switch normalized {
	case "verylow":
		return ConfidenceVeryLow, nil
	case "low":
		return ConfidenceLow, nil
	case "medium":
		return ConfidenceMedium, nil
	case "high":
		return ConfidenceHigh, nil
	case "veryhigh":
		return ConfidenceVeryHigh, nil
	default:
		return ConfidenceVeryLow, fmt.Errorf("unknown confidence %q", s)
	}
}
