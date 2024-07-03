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
	"sync"
)

// Logger initialized for package-wide use
var logger = log.Default()

// RuleError custom error for Rule
type RuleCreationError struct {
	Message string
}

func (e *RuleCreationError) Error() string {
	return fmt.Sprintf("Rule Creation failed with error: %s", e.Message)
}

type IRule interface {
	Evaluate(ctx context.Context, record map[string]interface{}) bool
}

type BaseRule struct {
	RuleOptions
	mu       sync.RWMutex
	Checksum string
}

// Disable disables a rule
func (r *BaseRule) Disable() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Disabled = true
}

func (r *BaseRule) CalculateChecksum() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.Checksum != "" {
		return r.Checksum
	}

	fset := token.NewFileSet()
	funcName := runtime.FuncForPC(reflect.ValueOf(r.EvaluateLogic).Pointer()).Name()
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
	r.Checksum = hex.EncodeToString(h.Sum(nil))
	return r.Checksum
}

// ApplyMatchers applies all enrichment functions to the event.
func (r *BaseRule) ApplyMatchers(ctx context.Context, record map[string]interface{}) bool {
	for _, matcher := range r.Matchers {
		if !matcher.Match(ctx, record) {
			return false
		}
	}
	return true
}

// ApplyEnrichments applies all enrichment functions to the event.
func (r *BaseRule) ApplyEnrichments(ctx context.Context, record map[string]interface{}) error {
	for _, enrich := range r.Enrichments {
		enrich.Enrich(ctx, record)
	}
	return nil
}

// ApplyTuningRules applies all tuning rules to the event.
func (r *BaseRule) ApplyTuningRules(ctx context.Context, record map[string]interface{}) error {
	for _, tuningRule := range r.TuningRules {
		tuningRule.Tune(ctx, record)
	}
	return nil
}

// ApplyPublishers applies all publishers to the event.
func (r *BaseRule) ApplyPublishers(ctx context.Context, record map[string]interface{}) error {
	for _, publisher := range r.Publishers {
		publisher.Publish(ctx, record)
	}
	return nil
}

// ApplyDispatchers applies all dispatchers to the event.
func (r *BaseRule) ApplyDispatchers(ctx context.Context, record map[string]interface{}) error {
	for _, dispatcher := range r.Dispatchers {
		dispatcher.Dispatch(ctx, record)
	}
	return nil
}

func (r *BaseRule) Evaluate(ctx context.Context, record map[string]interface{}) bool {
	return r.EvaluateLogic(ctx, record)
}

func (r *BaseRule) EvaluateLogic(ctx context.Context, record map[string]interface{}) bool {
	return true
}

func (r *BaseRule) Init(name string, setters ...RuleOption) {
	// Default Options
	ruleOpts := &RuleOptions{
		Name: name,
	}
	for _, setter := range setters {
		setter(ruleOpts)
	}
	r.RuleOptions = *ruleOpts
	r.Checksum = r.CalculateChecksum()
}
