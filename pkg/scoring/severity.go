package scoring

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Severity int

const (
	SeverityInfo Severity = iota
	SeverityLow
	SeverityMedium
	SeverityHigh
	SeverityCritical
)

var SeverityEnum = struct {
	Info, Low, Medium, High, Critical Severity
}{
	Info:     SeverityInfo,
	Low:      SeverityLow,
	Medium:   SeverityMedium,
	High:     SeverityHigh,
	Critical: SeverityCritical,
}

func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityLow:
		return "low"
	case SeverityMedium:
		return "medium"
	case SeverityHigh:
		return "high"
	case SeverityCritical:
		return "critical"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

func (s Severity) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

func (s *Severity) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	v, err := ParseSeverity(str)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

func IsValidSeverity(s Severity) bool {
	return s >= SeverityInfo && s <= SeverityCritical
}

func ParseSeverity(s string) (Severity, error) {
	switch strings.ToLower(s) {
	case "info":
		return SeverityInfo, nil
	case "low":
		return SeverityLow, nil
	case "medium":
		return SeverityMedium, nil
	case "high":
		return SeverityHigh, nil
	case "critical":
		return SeverityCritical, nil
	default:
		return SeverityInfo, fmt.Errorf("unknown severity %q", s)
	}
}
