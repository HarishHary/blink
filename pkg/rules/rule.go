package rules

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"runtime"
	"time"

	"github.com/google/uuid"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/events"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/scoring"
)

type IRule interface {
	// Interface methods that need to be implemented
	Evaluate(event events.Event) (bool, errors.Error)
	DynamicSeverity(event events.Event) scoring.Severity
	Dedup(event events.Event) []string
	AlertTitle(event events.Event) string
	AlertDescription(event events.Event) string
	AlertContext(event events.Event) map[string]any

	// Getters
	Id() string
	Name() string
	Description() string
	Enabled() bool
	FileName() string
	DisplayName() string
	References() []string
	Severity() scoring.Severity
	Confidence() scoring.Confidence
	RiskScore() scoring.RiskScore
	MergeByKeys() []string
	MergeWindowMins() time.Duration
	ReqSubkeys() []string
	Signal() bool
	SignalThreshold() scoring.Confidence

	Tags() []string
	Dispatchers() []string
	LogTypes() []string
	Observables() []Observables
	Matchers() []string
	Formatters() []string
	Enrichments() []string
	TuningRules() []string
	Checksum() string
	ApplyMatchers(event events.Event) bool
	SubKeysInEvent(event events.Event) bool
}

type Observables struct {
	description string
	name        string
	aggregation bool
}

func (o *Observables) Description() string {
	return o.description
}

func (o *Observables) Name() string {
	return o.name
}

func (o *Observables) Aggregation() bool {
	return o.aggregation
}

type Rule struct {
	id              string
	name            string
	description     string
	enabled         bool
	fileName        string
	displayName     string
	references      []string
	severity        scoring.Severity
	confidence      scoring.Confidence
	riskScore       scoring.RiskScore
	mergeByKeys     []string
	mergeWindowMins time.Duration
	reqSubkeys      []string
	signal          bool
	signalThreshold scoring.Confidence
	tags            []string
	dispatchers     []string
	logTypes        []string
	observables     []Observables
	matchers        []string
	formatters      []string
	enrichments     []string
	tuningRules     []string
	checksum        string
}

func (r *Rule) Id() string {
	return r.id
}

func (r *Rule) Name() string {
	return r.name
}

func (r *Rule) Description() string {
	return r.description
}

func (r *Rule) Enabled() bool {
	return r.enabled
}

func (r *Rule) FileName() string {
	return r.fileName
}

func (r *Rule) DisplayName() string {
	return r.displayName
}

func (r *Rule) References() []string {
	return r.references
}

func (r *Rule) Severity() scoring.Severity {
	return r.severity
}

func (r *Rule) Confidence() scoring.Confidence {
	return r.confidence
}

func (r *Rule) RiskScore() scoring.RiskScore {
	return r.riskScore
}

func (r *Rule) SignalThreshold() scoring.Confidence {
	return r.signalThreshold
}

func (r *Rule) MergeByKeys() []string {
	return r.mergeByKeys
}

func (r *Rule) MergeWindowMins() time.Duration {
	return r.mergeWindowMins
}

func (r *Rule) ReqSubkeys() []string {
	return r.reqSubkeys
}

func (r *Rule) Signal() bool {
	return r.signal
}

func (r *Rule) Tags() []string {
	return r.tags
}

func (r *Rule) Dispatchers() []string {
	return r.dispatchers
}

func (r *Rule) LogTypes() []string {
	return r.logTypes
}

func (r *Rule) Observables() []Observables {
	return r.observables
}

func (r *Rule) Matchers() []string {
	return r.matchers
}

func (r *Rule) Formatters() []string {
	return r.formatters
}

func (r *Rule) Enrichments() []string {
	return r.enrichments
}

func (r *Rule) TuningRules() []string {
	return r.tuningRules
}

func (r *Rule) Disable() {
	r.enabled = false
}

func (r *Rule) DynamicSeverity(event events.Event) scoring.Severity {
	return r.severity
}

func (r *Rule) Dedup(event events.Event) []string {
	return r.mergeByKeys
}

func (r *Rule) AlertDescription(event events.Event) string {
	return r.description
}

func (r *Rule) AlertTitle(event events.Event) string {
	return r.name
}

func (r *Rule) AlertContext(event events.Event) map[string]any {
	return nil
}

func (r *Rule) Checksum() string {
	if r.checksum != "" {
		return r.checksum
	}

	fset := token.NewFileSet()
	funcName := runtime.FuncForPC(reflect.ValueOf(r.Evaluate).Pointer()).Name()
	node, err := parser.ParseFile(fset, "", fmt.Sprintf("package main; var f = %s", funcName), parser.ParseComments)
	if err != nil {
		return "n/a"
	}

	h := md5.New()
	ast.Inspect(node, func(n ast.Node) bool {
		if expr, ok := n.(*ast.ExprStmt); ok {
			h.Write([]byte(expr.X.(*ast.BasicLit).Value))
		}
		return true
	})
	r.checksum = hex.EncodeToString(h.Sum(nil))
	return r.checksum
}

// // ApplyMatchers applies all matchers to the event.
func (r *Rule) ApplyMatchers(event events.Event) bool {
	if !r.enabled {
		return false
	}
	matchersRepository := matchers.GetMatcherRepository()

	for _, matcher := range r.matchers {
		matcher, err := matchersRepository.Get(matcher)
		if err != nil {
			// TODO: log error and continue
			continue
		}
		if !matcher.Enabled() {
			// TODO: log disabled matcher and continue
			continue
		}
		match, err := matcher.Match(event)
		if err != nil {
			// TODO: log error and continue
			continue
		}
		if !match {
			return false // If any matcher fails, do not apply the rule
		}
	}
	return true
}

func (r *Rule) SubKeysInEvent(event events.Event) bool {
	if !r.enabled {
		return false
	}

	for _, subkey := range r.reqSubkeys {
		value := event.Get(subkey, nil)
		if value == nil {
			return false
		}
	}
	return true
}

func (r *Rule) Evaluate(event events.Event) (bool, errors.Error) {
	return true, nil
}

func NewRule(name string, optFns ...RuleOptions) (*Rule, errors.Error) {
	if name == "" {
		return nil, errors.New("invalid rule options")
	}
	rule := &Rule{
		name:            name,
		description:     "Unknown description",
		id:              uuid.NewString(),
		severity:        scoring.SeverityEnum.Info,
		confidence:      scoring.ConfidenceEnum.VeryLow,
		enabled:         true,
		signal:          true,
		mergeWindowMins: 0,
	}
	for _, optFn := range optFns {
		optFn(rule)
	}
	rule.riskScore = scoring.ComputeRiskScore(rule.confidence, rule.severity)
	return rule, nil
}
