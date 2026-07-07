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
	"github.com/harishhary/blink/pkg/events"
)

type Pool struct {
	*pools.ProcessPool[Rule]
}

func NewPool(cfg config.Source[*RuleMetadata], drainTimeout time.Duration) *Pool {
	return &Pool{
		ProcessPool: pools.NewProcessPool[Rule](config.RolloutFor(cfg), pools.NewPoolMetrics("rules"), drainTimeout),
	}
}

// Evaluate runs all evts against the rule identified by ruleID. When the pool that will serve the
// batch has more than one worker (max_procs > 1) and there are enough events, the batch is sharded
// into up to that many contiguous chunks evaluated concurrently - each chunk acquires its own
// subprocess - and the per-chunk results are concatenated back in original order. With a single-worker
// pool (or too few events) it is one call, identical to the pre-sharding behaviour.
//
// The shard count is sized off the serving pool (via ServingPoolSize with the canary hash key), so a
// canary candidate is exercised at its own max_procs rather than the stable version's.
func (p *Pool) Evaluate(ctx context.Context, ruleID string, evts []events.Event, canaryHashKey string) ([]EventResult, errors.Error) {
	// Shadow candidate (if any): a separate fan-out at the candidate's own max_procs, results dropped.
	p.shadowEvaluate(ctx, ruleID, evts)

	k := p.ServingPoolSize(ruleID, canaryHashKey)
	if k > len(evts) {
		k = len(evts)
	}
	if k <= 1 {
		// Single-worker pool, or too few events to bother sharding.
		return p.evaluateChunk(ctx, ruleID, evts, canaryHashKey)
	}

	chunks := shardEvents(evts, k)
	out := make([][]EventResult, len(chunks))
	errs := make([]errors.Error, len(chunks))
	var wg sync.WaitGroup
	for i := range chunks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i], errs[i] = p.evaluateChunk(ctx, ruleID, chunks[i], canaryHashKey)
		}(i)
	}
	wg.Wait()

	// Any shard error fails the whole Evaluate (mirrors the single-call path). First error wins.
	total := 0
	for i, e := range errs {
		if e != nil {
			return nil, e
		}
		total += len(out[i])
	}
	results := make([]EventResult, 0, total)
	for _, r := range out {
		results = append(results, r...)
	}
	return results, nil
}

// evaluateChunk is one production pool call: it acquires a subprocess (stable, or the canary candidate
// for the hashed slice) and evaluates evts against ruleID. The shadow candidate is driven separately by
// shadowEvaluate.
func (p *Pool) evaluateChunk(ctx context.Context, ruleID string, evts []events.Event, canaryHashKey string) ([]EventResult, errors.Error) {
	var results []EventResult
	prodFn := func(callCtx context.Context, r Rule) error {
		if !r.RuleMetadata().Enabled {
			results = make([]EventResult, len(evts))
			return nil
		}
		var e errors.Error
		results, e = r.Evaluate(callCtx, evts)
		return e
	}
	err := p.Call(ctx, ruleID, canaryHashKey, prodFn)
	if err != nil {
		return nil, errors.NewE(err)
	}
	return results, nil
}

// shadowEvaluate fans the full batch out to the shadow candidate (if ruleID is in shadow mode) at the
// candidate's own max_procs, each shard a detached CallShadow whose result is dropped. No-op otherwise.
// Events are read-only (World B: the executor builds alerts on fresh maps), so shards share the batch.
func (p *Pool) shadowEvaluate(ctx context.Context, ruleID string, evts []events.Event) {
	sk := p.ShadowPoolSize(ruleID)
	if sk == 0 || len(evts) == 0 {
		return
	}
	for _, chunk := range shardEvents(evts, sk) {
		chunk := chunk
		p.CallShadow(ctx, ruleID, func(callCtx context.Context, r Rule) error {
			if !r.RuleMetadata().Enabled {
				return nil
			}
			_, e := r.Evaluate(callCtx, chunk)
			return e
		})
	}
}

// shardEvents splits evts into k contiguous, near-equal chunks (k >= 1), preserving order so that
// concatenating the per-chunk results realigns 1:1 with the original event indices. The first
// (n % k) chunks get one extra element; empty chunks are omitted.
func shardEvents(evts []events.Event, k int) [][]events.Event {
	if k < 1 {
		k = 1
	}
	n := len(evts)
	base := n / k
	rem := n % k
	chunks := make([][]events.Event, 0, k)
	start := 0
	for i := 0; i < k; i++ {
		size := base
		if i < rem {
			size++
		}
		if size == 0 {
			continue
		}
		chunks = append(chunks, evts[start:start+size])
		start += size
	}
	return chunks
}

// Sync applies plugin lifecycle messages (register/update/unregister/remove/migrate) to the pool.
func (p *Pool) Sync(msg messaging.Message) { plugin.SyncPool(p.ProcessPool, msg) }
