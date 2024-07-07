package aws_matchers

import (
	"github.com/harishhary/blink/src/shared/matchers"
)

type AwsConfigConfigComplianceMatcher struct {
	matchers.Matcher
}

// IsConfigCompliance checks if the record event is from config compliance
func (m *AwsConfigConfigComplianceMatcher) MatchLogic(record map[string]interface{}) bool {
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

type AwsConfigAutoRemediationMatcher struct {
	matchers.Matcher
}

// IsAutoRemediation checks if the record is an auto-remediation event
func (m *AwsConfigAutoRemediationMatcher) MatchLogic(record map[string]interface{}) bool {
	if eventName, ok := record["eventName"].(string); ok && eventName == "StartAutomationExecution" {
		if eventSource, ok := record["eventSource"].(string); ok && eventSource == "ssm.amazonaws.com" {
			if sourceIPAddress, ok := record["sourceIPAddress"].(string); ok && sourceIPAddress == "config.amazonaws.com" {
				return true
			}
		}
	}
	return false
}
