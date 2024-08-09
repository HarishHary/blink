// Define the production accounts
package aws_matchers

import (
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
)

// AwsGuardDutyMatcher contains matchers for AWS GuardDuty service
type AwsGuardDutyMatcher struct {
	matchers.Matcher
}

// GuardDuty checks if the event is a GuardDuty finding
func (m *AwsGuardDutyMatcher) MatchLogic(event events.Event) bool {
	if detailType, ok := event["detail-type"].(string); ok {
		return detailType == "GuardDuty Finding"
	}
	return false
}

// Export the plugin as a symbol
var MatcherPlugin AwsGuardDutyMatcher
