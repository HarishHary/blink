package scoring

import (
	"encoding/json"
	"fmt"
	"strings"
)

type RiskScore int

const (
	RiskScoreLow RiskScore = iota
	RiskScoreMedium
	RiskScoreHigh
	RiskScoreCritical
)

var RiskScoreEnum = struct {
	Low, Medium, High, Critical RiskScore
}{
	Low:      RiskScoreLow,
	Medium:   RiskScoreMedium,
	High:     RiskScoreHigh,
	Critical: RiskScoreCritical,
}

func (r RiskScore) String() string {
	switch r {
	case RiskScoreLow:
		return "low"
	case RiskScoreMedium:
		return "medium"
	case RiskScoreHigh:
		return "high"
	case RiskScoreCritical:
		return "critical"
	default:
		return fmt.Sprintf("riskscore(%d)", int(r))
	}
}

func (r RiskScore) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

func (r *RiskScore) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	switch strings.ToLower(str) {
	case "low":
		*r = RiskScoreLow
	case "medium":
		*r = RiskScoreMedium
	case "high":
		*r = RiskScoreHigh
	case "critical":
		*r = RiskScoreCritical
	default:
		return fmt.Errorf("unknown risk score %q", str)
	}
	return nil
}

var riskMatrix = [5][5]RiskScore{
	// VeryLow:  Info         Low            Medium         High           Critical
	{RiskScoreLow, RiskScoreLow, RiskScoreLow, RiskScoreMedium, RiskScoreMedium},
	// Low:      Info         Low            Medium         High           Critical
	{RiskScoreLow, RiskScoreLow, RiskScoreMedium, RiskScoreMedium, RiskScoreMedium},
	// Medium:   Info         Low            Medium         High           Critical
	{RiskScoreLow, RiskScoreMedium, RiskScoreMedium, RiskScoreHigh, RiskScoreHigh},
	// High:     Info         Low            Medium         High           Critical
	{RiskScoreLow, RiskScoreMedium, RiskScoreHigh, RiskScoreHigh, RiskScoreCritical},
	// VeryHigh: Info         Low            Medium         High           Critical
	{RiskScoreMedium, RiskScoreHigh, RiskScoreHigh, RiskScoreCritical, RiskScoreCritical},
}

// ComputeRiskScore derives a RiskScore from confidence x severity levels.
func ComputeRiskScore(confidence Confidence, severity Severity) RiskScore {
	if !IsValidConfidence(confidence) || !IsValidSeverity(severity) {
		return RiskScoreLow
	}
	return riskMatrix[confidence][severity]
}
