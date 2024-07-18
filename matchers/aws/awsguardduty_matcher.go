// Define the production accounts
package aws_matchers

import (
	"github.com/harishhary/blink/src/shared"
	"github.com/harishhary/blink/src/shared/matchers"
)

// AwsGuardDutyMatcher contains matchers for AWS GuardDuty service
type AwsGuardDutyMatcher struct {
	matchers.Matcher
}

// GuardDuty checks if the record is a GuardDuty finding
func (m *AwsGuardDutyMatcher) MatchLogic(record shared.Record) bool {
	if detailType, ok := record["detail-type"].(string); ok {
		return detailType == "GuardDuty Finding"
	}
	return false
}

// Export the plugin as a symbol
var MatcherPlugin AwsGuardDutyMatcher
