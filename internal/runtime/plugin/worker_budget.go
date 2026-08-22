package plugin

import "sync/atomic"

// WorkerBudget caps the plugin workers one process may run past every deployment's reserved
// min_procs, because those workers are subprocesses competing for the same cores.
type WorkerBudget struct {
	max      int64
	growth   atomic.Int64
	reserved atomic.Int64
}

// NewWorkerBudget returns a budget allowing limit workers past the reservations, and none below one.
func NewWorkerBudget(limit int) *WorkerBudget {
	return &WorkerBudget{max: int64(max(0, limit))}
}

// limit reports how many workers the budget allows past the reservations.
func (b *WorkerBudget) limit() int {
	if b == nil {
		return 0
	}
	return int(b.max)
}

// acquire takes one worker from the budget and reports whether it had room; a nil budget is unbounded.
func (b *WorkerBudget) acquire() bool {
	if b == nil {
		return true
	}
	if b.growth.Add(1) > b.max {
		b.growth.Add(-1)
		return false
	}
	return true
}

// release returns n workers to the budget, so a shrinking deployment lets another one grow.
func (b *WorkerBudget) release(n int) {
	if b == nil || n <= 0 {
		return
	}
	b.growth.Add(-int64(n))
}

// reserve adds delta to the workers deployments keep whatever the budget says, negative to give
// them back, and reports the process total.
func (b *WorkerBudget) reserve(delta int) int {
	if b == nil {
		return 0
	}
	return int(b.reserved.Add(int64(delta)))
}
