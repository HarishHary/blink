package rules

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"reflect"
	"runtime"

	"github.com/harishhary/blink/src/shared"
	"github.com/harishhary/blink/src/shared/dispatchers"
	"github.com/harishhary/blink/src/shared/enrichments"
	"github.com/harishhary/blink/src/shared/formatters"
	"github.com/harishhary/blink/src/shared/inputs"
	"github.com/harishhary/blink/src/shared/matchers"
	"github.com/harishhary/blink/src/shared/rules/tuning_rules"
)

// Logger initialized for package-wide use
var logger = log.Default()

// RuleError custom error for Rule
type RuleError struct {
	Message string
}

func (e *RuleError) Error() string {
	return fmt.Sprintf("Rule failed with error: %s", e.Message)
}

type IRule interface {
	Evaluate(ctx context.Context, record shared.Record) bool
	Name() string
}

type Rule struct {
	name            string
	id              string
	description     string
	severity        int
	mergeByKeys     []string
	mergeWindowMins int
	reqSubkeys      []string
	disabled        bool
	inputs          []inputs.IInput
	dispatchers     []dispatchers.IDispatcher
	matchers        []matchers.IMatcher
	formatters      []formatters.IFormatter
	enrichments     []enrichments.IEnrichment
	tuningRules     []tuning_rules.ITuningRule
	checksum        string
}

func (r *Rule) Disable() {
	r.disabled = true
}

func (r *Rule) Name() string {
	return r.name
}

func (r *Rule) Checksum() string {
	if r.checksum != "" {
		return r.checksum
	}

	fset := token.NewFileSet()
	funcName := runtime.FuncForPC(reflect.ValueOf(r.Evaluate).Pointer()).Name()
	node, err := parser.ParseFile(fset, "", fmt.Sprintf("package main; var f = %s", funcName), parser.ParseComments)
	if err != nil {
		logger.Printf("Could not parse rule function: %v", err)
		return "checksum unknown"
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

// ApplyMatchers applies all matchers to the event.
func (r *Rule) ApplyMatchers(ctx context.Context, record shared.Record) bool {
	if r.disabled {
		return false
	}

	for _, matcher := range r.matchers {
		match, err := matcher.Match(ctx, record)
		if err != nil {
			return false
		}
		if !match {
			return false // If any matcher fails, do not apply the rule
		}
	}
	return true
}

// ApplyEnrichments applies all enrichment functions to the event.
func (r *Rule) ApplyEnrichments(ctx context.Context, record shared.Record) error {
	for _, enrich := range r.enrichments {
		enrich.Enrich(ctx, record)
	}
	return nil
}

// ApplyTuningRules applies all tuning rules to the event.
func (r *Rule) ApplyTuningRules(ctx context.Context, record shared.Record) error {
	for _, tuningRule := range r.tuningRules {
		tuningRule.Tune(ctx, record)
	}
	return nil
}

// ApplyFormatters applies all formatters to the event.
func (r *Rule) ApplyFormatters(ctx context.Context, record shared.Record) error {
	for _, formatter := range r.formatters {
		formatter.Format(ctx, record)
	}
	return nil
}

// ApplyDispatchers applies all dispatchers to the event.
func (r *Rule) ApplyDispatchers(ctx context.Context, record shared.Record) error {
	for _, dispatcher := range r.dispatchers {
		dispatcher.Dispatch(ctx, record)
	}
	return nil
}

func (r *Rule) Evaluate(ctx context.Context, record shared.Record) bool {
	return true
}

func NewRule(name string, setters ...RuleOption) Rule {
	// Default Options
	r := Rule{
		name: name,
	}
	for _, setter := range setters {
		setter(&r)
	}
	return r
}
