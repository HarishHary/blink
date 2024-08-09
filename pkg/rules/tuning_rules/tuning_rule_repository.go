package tuning_rules

import (
	"github.com/harishhary/blink/internal/repository"
)

type TuningRuleRepository struct {
	*repository.Repository[ITuningRule]
}

var tuningRuleRepository *TuningRuleRepository

func init() {
	tuningRuleRepository = NewTuningRuleRepository()
}

func GetTuningRuleRepository() *TuningRuleRepository {
	return tuningRuleRepository
}

func NewTuningRuleRepository() *TuningRuleRepository {
	return &TuningRuleRepository{
		Repository: repository.NewRepository[ITuningRule](),
	}
}

func (r *TuningRuleRepository) GetGlobalTuningRules() []ITuningRule {
	var tuningRules []ITuningRule
	for _, rule := range r.Data {
		if rule.Global() && rule.Enabled() {
			tuningRules = append(tuningRules, rule)
		}
	}
	return tuningRules
}
