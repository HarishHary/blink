package plugin

import (
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync/atomic"
)

const (
	DefaultPluginProcessMemoryFootprint = 64 << 20  // 64MB per plugin subprocess
	DefaultProcessBudgetMemoryReserve   = 256 << 20 // 256MB left for the service process itself
	DefaultProcessBudgetHardCap         = 512       // sanity ceiling regardless of memory available
)

// processBudgetFromResources sizes the default ProcessBudget from CPU and memory together.
// GOMAXPROCS alone undercounts it for the common case here: small, mostly-idle plugin
// subprocesses (a matcher's whole job can be one comparison) that spend far more time on a gRPC
// round trip than on CPU, so cores are not the binding constraint - memory is, since running past
// the container's limit OOMs the pod regardless of how idle each subprocess is.
func processBudgetFromResources() int {
	cpuFloor := goruntime.GOMAXPROCS(0) * DefaultRuntimeProcessGrowthPerProc
	limit, ok := cgroupMemoryLimitBytes()
	if !ok {
		return cpuFloor
	}
	available := limit - DefaultProcessBudgetMemoryReserve
	if available <= 0 {
		return cpuFloor
	}
	memoryBudget := min(int(available/DefaultPluginProcessMemoryFootprint), DefaultProcessBudgetHardCap)
	return max(cpuFloor, memoryBudget)
}

// cgroupMemoryLimitBytes reads this container's memory limit: cgroup v2's memory.max, falling
// back to cgroup v1's memory.limit_in_bytes.
func cgroupMemoryLimitBytes() (int64, bool) {
	const unlimitedV1Threshold = 1 << 62
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "max" {
			return 0, false
		}
		limit, err := strconv.ParseInt(text, 10, 64)
		if err != nil || limit <= 0 || limit >= unlimitedV1Threshold {
			continue
		}
		return limit, true
	}
	return 0, false
}

// ProcessBudget caps the plugin processes one service process may run past every deployment's
// reserved min_procs - sized by processBudgetFromResources when a caller leaves it unset.
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
