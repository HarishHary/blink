package plugin

import "sync/atomic"

// ProcessBudget caps the plugin processes one service process may run past every deployment's
// reserved min_procs, because those plugin processes are subprocesses competing for the same cores.
type ProcessBudget struct {
	max      int64
	growth   atomic.Int64
	reserved atomic.Int64
}

// NewProcessBudget returns a budget allowing limit plugin processes past the reservations, and none below one.
func NewProcessBudget(limit int) *ProcessBudget {
	return &ProcessBudget{max: int64(max(0, limit))}
}

// limit reports how many plugin processes the budget allows past the reservations.
func (b *ProcessBudget) limit() int {
	if b == nil {
		return 0
	}
	return int(b.max)
}

// acquire takes one plugin process from the budget and reports whether it had room; a nil budget is unbounded.
func (b *ProcessBudget) acquire() bool {
	if b == nil {
		return true
	}
	if b.growth.Add(1) > b.max {
		b.growth.Add(-1)
		return false
	}
	return true
}

// release returns n plugin processes to the budget, so a shrinking deployment lets another one grow.
func (b *ProcessBudget) release(n int) {
	if b == nil || n <= 0 {
		return
	}
	b.growth.Add(-int64(n))
}

// reserve adds delta to the plugin processes deployments keep whatever the budget says, negative to give
// them back, and reports the process total.
func (b *ProcessBudget) reserve(delta int) int {
	if b == nil {
		return 0
	}
	return int(b.reserved.Add(int64(delta)))
}
