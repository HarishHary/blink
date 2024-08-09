package scoring

type RiskScore = string

var RiskScoreEnum = struct {
	Low      RiskScore
	Medium   RiskScore
	High     RiskScore
	Critical RiskScore
}{
	Low:      "low",
	Medium:   "medium",
	High:     "high",
	Critical: "critical",
}

var riskMatrix = map[Confidence]map[Severity]string{
	ConfidenceEnum.VeryHigh: {
		SeverityEnum.Info:     RiskScoreEnum.Medium,
		SeverityEnum.Low:      RiskScoreEnum.High,
		SeverityEnum.Medium:   RiskScoreEnum.High,
		SeverityEnum.High:     RiskScoreEnum.Critical,
		SeverityEnum.Critical: RiskScoreEnum.Critical,
	},
	ConfidenceEnum.High: {
		SeverityEnum.Info:     RiskScoreEnum.Low,
		SeverityEnum.Low:      RiskScoreEnum.Medium,
		SeverityEnum.Medium:   RiskScoreEnum.High,
		SeverityEnum.High:     RiskScoreEnum.High,
		SeverityEnum.Critical: RiskScoreEnum.Critical,
	},
	ConfidenceEnum.Medium: {
		SeverityEnum.Info:     RiskScoreEnum.Low,
		SeverityEnum.Low:      RiskScoreEnum.Medium,
		SeverityEnum.Medium:   RiskScoreEnum.Medium,
		SeverityEnum.High:     RiskScoreEnum.High,
		SeverityEnum.Critical: RiskScoreEnum.High,
	},
	ConfidenceEnum.Low: {
		SeverityEnum.Info:     RiskScoreEnum.Low,
		SeverityEnum.Low:      RiskScoreEnum.Low,
		SeverityEnum.Medium:   RiskScoreEnum.Medium,
		SeverityEnum.High:     RiskScoreEnum.Medium,
		SeverityEnum.Critical: RiskScoreEnum.Medium,
	},
	ConfidenceEnum.VeryLow: {
		SeverityEnum.Info:     RiskScoreEnum.Low,
		SeverityEnum.Low:      RiskScoreEnum.Low,
		SeverityEnum.Medium:   RiskScoreEnum.Low,
		SeverityEnum.High:     RiskScoreEnum.Medium,
		SeverityEnum.Critical: RiskScoreEnum.Medium,
	},
}

// Function to compute the risk score
func ComputeRiskScore(confidence Confidence, severity Severity) string {
	if severityMap, exists := riskMatrix[confidence]; exists {
		if riskScore, exists := severityMap[severity]; exists {
			return riskScore
		}
	}
	return "Unknown"
}
