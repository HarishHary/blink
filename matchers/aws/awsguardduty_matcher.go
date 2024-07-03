// Define the production accounts
package matchers

import (
	"github.com/harishhary/blink/src/matchers"
)

// AwsGuardDutyMatcher contains matchers for AWS GuardDuty service
type AwsGuardDutyMatcher struct {
	matchers.BaseMatcher
}

// GuardDuty checks if the record is a GuardDuty finding
func (m *AwsGuardDutyMatcher) GuardDuty(record map[string]interface{}) bool {
	if detailType, ok := record["detail-type"].(string); ok {
		return detailType == "GuardDuty Finding"
	}
	return false
}
