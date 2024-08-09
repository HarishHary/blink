package scoring

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
	for _, value := range []Severity{
		SeverityEnum.Info,
		SeverityEnum.Low,
		SeverityEnum.Medium,
		SeverityEnum.High,
		SeverityEnum.Critical,
	} {
		if severity == value {
			return true
		}
	}
	return false
}
