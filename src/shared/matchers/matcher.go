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

type IMatcher interface {
	Match(ctx context.Context, record map[string]interface{}) (bool, error)
}

type Matcher struct {
	Name        string
	Description string
}

func (r *Matcher) Match(ctx context.Context, record map[string]interface{}) (bool, error) {
	log.Printf("Using matcher %s with context:%s and record:%s", r.Name, ctx, record)
	return r.MatchLogic(ctx, record)
}

func (r *Matcher) MatchLogic(ctx context.Context, record map[string]interface{}) (bool, error) {
	log.Printf("Using matcher %s with context:%s and record:%s", r.Name, ctx, record)
	return true, nil
}
