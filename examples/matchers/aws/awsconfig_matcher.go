package aws_matchers

import (
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
)

type AwsConfigConfigComplianceMatcher struct {
	matchers.Matcher
}

// IsConfigCompliance checks if the record event is from config compliance
func (m *AwsConfigConfigComplianceMatcher) Match(event events.Event) bool {
	if eventSource, ok := event["eventSource"].(string); ok && eventSource == "config.amazonaws.com" {
		if eventName, ok := event["eventName"].(string); ok && eventName == "PutEvaluations" {
			if requestParameters, ok := event["requestParameters"].(map[string]any); ok {
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
func (m *AwsConfigAutoRemediationMatcher) Match(event events.Event) bool {
	if eventName, ok := event["eventName"].(string); ok && eventName == "StartAutomationExecution" {
		if eventSource, ok := event["eventSource"].(string); ok && eventSource == "ssm.amazonaws.com" {
			if sourceIPAddress, ok := event["sourceIPAddress"].(string); ok && sourceIPAddress == "config.amazonaws.com" {
				return true
			}
		}
	}
	return false
}
