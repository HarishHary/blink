package matchers

import (
	"context"
	"fmt"
	"log"
)

// MatcherError custom error for Matcher
type MatcherError struct {
	Message string
}

func (e *MatcherError) Error() string {
	return fmt.Sprintf("Matcher failed with error: %s", e.Message)
}

type IMatchers interface {
	Match(ctx context.Context, record map[string]interface{}) bool
}

type BaseMatcher struct {
	Name string
}

func (r *BaseMatcher) Match(ctx context.Context, record map[string]interface{}) bool {
	log.Printf("Using matcher %s with context:%s and record:%s", r.Name, ctx, record)
	return r.MatchLogic(ctx, record)
}

func (r *BaseMatcher) MatchLogic(ctx context.Context, record map[string]interface{}) bool {
	log.Printf("Using matcher %s with context:%s and record:%s", r.Name, ctx, record)
	return true
}
