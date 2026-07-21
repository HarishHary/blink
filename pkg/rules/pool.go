package rules

import (
	"context"
	"sync"
	"time"

	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/pools"
	evts "github.com/harishhary/blink/pkg/events"
)

// Pool manages live rule plugin processes and request routing.
type Pool struct {
	*pools.ProcessPool[Rule]
}

// EvaluateResult holds the result from evaluating events.
type EvaluateResult struct {
	Items   []EvaluateItem
	CallErr errors.Error // whole-call failure; never record-scoped
}

// NewPool builds a rule process pool.
func NewPool(cfg config.Source[*RuleMetadata], drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: pools.NewProcessPool[Rule](config.RolloutFor(cfg), pools.NewPoolMetrics("rules"), drainTimeout),
	}
}

// Evaluate runs a batch of events against the named rule.
func (p *Pool) Evaluate(ctx context.Context, ruleID string, events []evts.Event) EvaluateResult {
	p.shadowEvaluate(ctx, ruleID, events)

	type routeGroup struct {
		rolloutKey string
		indexes    []int
		events     []evts.Event
	}
	groups := make([]routeGroup, 0)
	groupIndexByBucket := make(map[uint32]int)
	for i, event := range events {
		rolloutKey := pools.TenantRolloutKey(event["tenant_id"])
		bucket := pools.RolloutBucket(rolloutKey)
		groupIndex, ok := groupIndexByBucket[bucket]
		if !ok {
			groupIndex = len(groups)
			groupIndexByBucket[bucket] = groupIndex
			groups = append(groups, routeGroup{rolloutKey: rolloutKey})
		}
		groups[groupIndex].indexes = append(groups[groupIndex].indexes, i)
		groups[groupIndex].events = append(groups[groupIndex].events, event)
	}

	parts := make([]EvaluateResult, len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := p.ServingPoolSize(ruleID, group.rolloutKey)
			shards := pools.ShardConcurrent(group.events, k, func(chunk []evts.Event) EvaluateResult {
				return p.evaluateChunk(ctx, ruleID, chunk, group.rolloutKey)
			})
			part := EvaluateResult{Items: make([]EvaluateItem, 0, len(group.events))}
			for _, shard := range shards {
				if shard.CallErr != nil {
					parts[i] = EvaluateResult{CallErr: shard.CallErr}
					return
				}
				part.Items = append(part.Items, shard.Items...)
			}
			parts[i] = part
		}(i)
	}
	wg.Wait()

	result := EvaluateResult{Items: make([]EvaluateItem, len(events))}
	for i, part := range parts {
		if part.CallErr != nil {
			return EvaluateResult{CallErr: part.CallErr}
		}
		group := groups[i]
		if len(part.Items) != len(group.indexes) {
			return EvaluateResult{CallErr: errors.NewF("rule %s returned invalid routed result shape", ruleID)}
		}
		for j, inputIndex := range group.indexes {
			result.Items[inputIndex] = part.Items[j]
		}
	}
	return result
}

// evaluateChunk is one production pool call: it acquires a subprocess (stable, or the canary candidate
// for the hashed slice) and evaluates evts against ruleID. The shadow candidate is driven separately by
// shadowEvaluate.
func (p *Pool) evaluateChunk(ctx context.Context, ruleID string, eventsChunk []evts.Event, rolloutKey string) EvaluateResult {
	items := make([]EvaluateItem, len(eventsChunk))
	prodFn := func(callCtx context.Context, r Rule) error {
		if !r.RuleMetadata().Enabled {
			return nil
		}
		batchResult := r.EvaluateBatch(callCtx, eventsChunk)
		if batchResult.CallErr != nil {
			return batchResult.CallErr
		}
		if len(batchResult.Items) != len(eventsChunk) {
			return &errors.ResultCardinalityError{PluginKind: "rule", PluginID: ruleID, Field: "items", Expected: len(eventsChunk), Actual: len(batchResult.Items)}
		}
		copy(items, batchResult.Items)
		return nil
	}
	err := p.Call(ctx, ruleID, rolloutKey, prodFn)
	if err != nil {
		return EvaluateResult{CallErr: errors.NewE(err)}
	}
	return EvaluateResult{Items: items}
}

// shadowEvaluate fans the full batch out to the shadow candidate (if ruleID is in shadow mode) at the
// candidate's own max_procs, each shard a detached CallShadow whose result is dropped. No-op otherwise.
// Events are read-only (World B: the executor builds alerts on fresh maps), so shards share the batch.
func (p *Pool) shadowEvaluate(ctx context.Context, ruleID string, events []evts.Event) {
	sk := p.ShadowPoolSize(ruleID)
	if sk == 0 || len(events) == 0 {
		return
	}
	for _, evtsChunk := range pools.ShardSlice(events, sk) {
		p.CallShadow(ctx, ruleID, func(callCtx context.Context, r Rule) error {
			if !r.RuleMetadata().Enabled {
				return nil
			}
			result := r.EvaluateBatch(callCtx, evtsChunk)
			if result.CallErr != nil {
				return result.CallErr
			}
			for _, item := range result.Items {
				if item.Err != nil {
					return item.Err
				}
			}
			return nil
		})
	}
}

// Sync applies plugin lifecycle messages to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }
