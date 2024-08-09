package rules

import (
	"github.com/harishhary/blink/internal/repository"
)

type RuleRepository struct {
	*repository.Repository[IRule]
}

var ruleRepository *RuleRepository

func init() {
	ruleRepository = NewRuleRepository()
}

func GetRuleRepository() *RuleRepository {
	return ruleRepository
}

func NewRuleRepository() *RuleRepository {
	return &RuleRepository{
		Repository: repository.NewRepository[IRule](),
	}
}

func (repo *RuleRepository) GetRulesForLogType(logType string) []IRule {
	var rules []IRule
	for _, rule := range repo.Data {
		if rule.LogTypes() == nil {
			continue
		}
		for _, ruleLogType := range rule.LogTypes() {
			if ruleLogType == logType {
				rules = append(rules, rule)
			}
		}
	}
	return rules
}
