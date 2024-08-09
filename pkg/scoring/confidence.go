package scoring

type Confidence = string

var ConfidenceEnum = struct {
	VeryLow  Confidence
	Low      Confidence
	Medium   Confidence
	High     Confidence
	VeryHigh Confidence
}{
	VeryLow:  "verylow",
	Low:      "low",
	Medium:   "medium",
	High:     "high",
	VeryHigh: "veryhigh",
}

func IsValidConfidence(confidence Confidence) bool {
	for _, value := range []Confidence{
		ConfidenceEnum.VeryLow,
		ConfidenceEnum.Low,
		ConfidenceEnum.Medium,
		ConfidenceEnum.High,
		ConfidenceEnum.VeryHigh,
	} {
		if confidence == value {
			return true
		}
	}
	return false
}
