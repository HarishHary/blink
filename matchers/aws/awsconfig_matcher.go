package matchers

import (
	"github.com/harishhary/blink/src/matchers"
)

// AwsConfigMatcher contains matchers relevant to AWS Config
type AwsConfigMatcher struct {
	matchers.BaseMatcher
}

// IsConfigCompliance checks if the record event is from config compliance
func (m *AwsConfigMatcher) IsConfigCompliance(record map[string]interface{}) bool {
	if eventSource, ok := record["eventSource"].(string); ok && eventSource == "config.amazonaws.com" {
		if eventName, ok := record["eventName"].(string); ok && eventName == "PutEvaluations" {
			if requestParameters, ok := record["requestParameters"].(map[string]interface{}); ok {
				if testMode, ok := requestParameters["testMode"].(bool); ok {
					return !testMode
				}
			}
		}
	}
	return false
}

// IsAutoRemediation checks if the record is an auto-remediation event
func (m *AwsConfigMatcher) IsAutoRemediation(record map[string]interface{}) bool {
	if eventName, ok := record["eventName"].(string); ok && eventName == "StartAutomationExecution" {
		if eventSource, ok := record["eventSource"].(string); ok && eventSource == "ssm.amazonaws.com" {
			if sourceIPAddress, ok := record["sourceIPAddress"].(string); ok && sourceIPAddress == "config.amazonaws.com" {
				return true
			}
		}
	}
	return false
}
