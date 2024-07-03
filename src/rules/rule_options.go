package rules

import (
	"github.com/harishhary/blink/src/dispatchers"
	"github.com/harishhary/blink/src/enrichments"
	"github.com/harishhary/blink/src/inputs"
	"github.com/harishhary/blink/src/matchers"
	"github.com/harishhary/blink/src/publishers"
)

type RuleOptions struct {
	Name               string
	Description        string
	Severity           int
	MergeByKeys        []string
	MergeWindowMins    int
	ReqSubkeys         []string
	Disabled           bool
	Inputs             []inputs.IInput
	Dispatchers        []dispatchers.IDispatcher
	DynamicDispatchers []dispatchers.IDynamicDispatcher
	Matchers           []matchers.IMatchers
	Publishers         []publishers.IPublishers
	Enrichments        []enrichments.IEnrichmentFunction
	TuningRules        []ITuningRule
	InitialContext     *map[string]interface{}
	Context            *map[string]interface{}
}

type RuleOption func(*RuleOptions)

func Description(Description string) RuleOption {
	return func(rule *RuleOptions) {
		rule.Description = Description
	}
}

func Severity(Severity int) RuleOption {
	return func(rule *RuleOptions) {
		rule.Severity = Severity
	}
}

func MergeByKeys(MergeByKeys []string) RuleOption {
	return func(rule *RuleOptions) {
		rule.MergeByKeys = MergeByKeys
	}
}

func MergeWindowMins(MergeWindowMins int) RuleOption {
	return func(rule *RuleOptions) {
		rule.MergeWindowMins = MergeWindowMins
	}
}

func ReqSubkeys(ReqSubkeys []string) RuleOption {
	return func(rule *RuleOptions) {
		rule.ReqSubkeys = ReqSubkeys
	}
}

func Disabled(Disabled bool) RuleOption {
	return func(rule *RuleOptions) {
		rule.Disabled = Disabled
	}
}

func Inputs(Inputs []inputs.IInput) RuleOption {
	return func(rule *RuleOptions) {
		rule.Inputs = Inputs
	}
}

func Dispatchers(Dispatchers []dispatchers.IDispatcher) RuleOption {
	return func(rule *RuleOptions) {
		rule.Dispatchers = Dispatchers
	}
}

func DynamicDispatchers(DynamicDispatchers []dispatchers.IDynamicDispatcher) RuleOption {
	return func(rule *RuleOptions) {
		rule.DynamicDispatchers = DynamicDispatchers
	}
}
func Matchers(Matchers []matchers.IMatchers) RuleOption {
	return func(rule *RuleOptions) {
		rule.Matchers = Matchers
	}
}

func Publishers(Publishers []publishers.IPublishers) RuleOption {
	return func(rule *RuleOptions) {
		rule.Publishers = Publishers
	}
}

func Enrichments(Enrichments []enrichments.IEnrichmentFunction) RuleOption {
	return func(rule *RuleOptions) {
		rule.Enrichments = Enrichments
	}
}

func TuningRules(TuningRules []ITuningRule) RuleOption {
	return func(rule *RuleOptions) {
		rule.TuningRules = TuningRules
	}
}

func InitialContext(InitialContext *map[string]interface{}) RuleOption {
	return func(rule *RuleOptions) {
		rule.InitialContext = InitialContext
	}
}

func Context(Context *map[string]interface{}) RuleOption {
	return func(rule *RuleOptions) {
		rule.Context = Context
	}
}
