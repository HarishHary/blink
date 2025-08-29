package scoring

import "slices"

type Severity = string

var SeverityEnum = struct {
	Info     Severity
	Low      Severity
	Medium   Severity
	High     Severity
	Critical Severity
}{
	Info:     "info",
	Low:      "low",
	Medium:   "medium",
	High:     "high",
	Critical: "critical",
}

func IsValidSeverity(severity Severity) bool {
	return slices.Contains([]Severity{
		SeverityEnum.Info,
		SeverityEnum.Low,
		SeverityEnum.Medium,
		SeverityEnum.High,
		SeverityEnum.Critical,
	}, severity)
}
