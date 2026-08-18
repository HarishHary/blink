package plugin

import (
	"fmt"
	"maps"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// DeploymentPoolLifecycle remains the catalog-facing deployment manager lifecycle facade.
type DeploymentPoolLifecycle string

const (
	DeploymentPoolStarting   DeploymentPoolLifecycle = "starting"
	DeploymentPoolRunning    DeploymentPoolLifecycle = "running"
	DeploymentPoolRestarting DeploymentPoolLifecycle = "restarting"
	DeploymentPoolFailed     DeploymentPoolLifecycle = "failed"
	DeploymentPoolDraining   DeploymentPoolLifecycle = "draining"
	DeploymentPoolStopped    DeploymentPoolLifecycle = "stopped"
)

// deploymentPoolState tracks one pool process and its recovery state.
type deploymentPoolState struct {
	pid           gen.PID
	restart       *runtime.ScheduledBackoff
	status        deploymentPoolStatus
	resizePending bool
	expectedStop  bool
	recovering    bool
}

// deploymentPoolStatus preserves the existing router/catalog status contract.
type deploymentPoolStatus struct {
	lifecycle      DeploymentPoolLifecycle
	availability   runtime.Availability
	healthyWorkers int
	desiredWorkers int
	queueDepth     int
	activeCalls    int
	workers        map[gen.PID]deploymentWorkerStatus
}

// clone copies a pool status and its worker map.
func (s deploymentPoolStatus) clone() deploymentPoolStatus {
	clone := s
	clone.workers = make(map[gen.PID]deploymentWorkerStatus, len(s.workers))
	maps.Copy(clone.workers, s.workers)
	return clone
}

// sameDeploymentPoolStatus compares pool status snapshots.
func sameDeploymentPoolStatus(left, right deploymentPoolStatus) bool {
	if left.lifecycle != right.lifecycle ||
		left.availability != right.availability ||
		left.healthyWorkers != right.healthyWorkers ||
		left.desiredWorkers != right.desiredWorkers ||
		left.queueDepth != right.queueDepth ||
		left.activeCalls != right.activeCalls ||
		len(left.workers) != len(right.workers) {
		return false
	}
	for pid, worker := range left.workers {
		other, ok := right.workers[pid]
		if !ok || !sameDeploymentWorkerStatus(worker, other) {
			return false
		}
	}
	return true
}

// deploymentPool places deployment workers and forwards normal-priority traffic.
type deploymentPool[T Syncable] struct {
	act.Pool
	adapter    *Adapter[T]
	options    DeploymentPoolOptions
	deployment Deployment
	size       int64
	workers    map[gen.PID]deploymentWorkerStatus
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageDeploymentPoolAddWorker requests one additional pool worker.
type MessageDeploymentPoolAddWorker struct{}

// MessageDeploymentPoolRemoveWorker requests one fewer pool worker.
type MessageDeploymentPoolRemoveWorker struct{}

// MessageDeploymentPoolResized reports the result of a pool resize request.
type MessageDeploymentPoolResized struct {
	pool gen.PID
	size int64
	err  error
}

// MessageDeploymentPoolStatusChanged reports a pool status update to its manager.
type MessageDeploymentPoolStatusChanged struct {
	pool   gen.PID
	status deploymentPoolStatus
}

// ---------------------------------------------------------------------------
// Actor lifecycle
// ---------------------------------------------------------------------------

// Init configures the pool and creates its worker factory.
func (p *deploymentPool[T]) Init(...any) (act.PoolOptions, error) {
	p.options = deploymentPoolOptionsWithDefaults(p.options)
	p.size = p.options.InitialSize
	p.workers = make(map[gen.PID]deploymentWorkerStatus)
	p.publishStatus()
	return act.PoolOptions{
		PoolSize:          p.size,
		WorkerMailboxSize: 1,
		WorkerFactory: func() gen.ProcessBehavior {
			return &deploymentWorker[T]{
				adapter:    p.adapter,
				options:    p.options.WorkerOptions,
				deployment: p.deployment,
			}
		},
	}, nil
}

// ---------------------------------------------------------------------------
// Message handling
// ---------------------------------------------------------------------------

// HandleMessage processes worker state and manager resize requests.
func (p *deploymentPool[T]) HandleMessage(from gen.PID, message any) error {
	switch msg := message.(type) {
	case MessageDeploymentWorkerStatusChanged:
		if from != msg.worker {
			return nil
		}
		p.workers[msg.worker] = msg.status
		p.publishStatus()

	case MessageDeploymentWorkerStopped:
		if from == msg.worker && msg.pool == p.PID() {
			delete(p.workers, msg.worker)
			p.publishStatus()
			msg.pool = p.PID()
			_ = p.SendWithPriority(p.Parent(), msg, gen.MessagePriorityHigh)
		}

	case MessageDeploymentWorkerRestartExhausted:
		if _, ok := p.workers[from]; ok {
			_ = p.SendWithPriority(p.Parent(), msg, gen.MessagePriorityHigh)
		}

	case MessageStop:
		if from == p.Parent() {
			return gen.TerminateReasonNormal
		}

	case MessageDeploymentPoolAddWorker:
		if from != p.Parent() {
			return nil
		}
		var err error
		if p.size >= p.options.MaxSize {
			err = fmt.Errorf("deployment pool worker capacity reached")
		} else {
			var size int64
			size, err = p.AddWorkers(1)
			if err == nil {
				p.size = size
			}
		}
		p.publishStatus()
		_ = p.SendWithPriority(p.Parent(), MessageDeploymentPoolResized{pool: p.PID(), size: p.size, err: err}, gen.MessagePriorityHigh)

	case MessageDeploymentPoolRemoveWorker:
		if from != p.Parent() {
			return nil
		}
		var err error
		if p.size <= 1 {
			err = fmt.Errorf("deployment pool cannot remove final worker")
		} else {
			var size int64
			size, err = p.RemoveWorkers(1)
			if err == nil {
				p.size = size
			}
		}
		p.publishStatus()
		_ = p.SendWithPriority(p.Parent(), MessageDeploymentPoolResized{pool: p.PID(), size: p.size, err: err}, gen.MessagePriorityHigh)
	}
	return nil
}

// HandleCall rejects unsupported synchronous pool calls.
func (p *deploymentPool[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("actorruntime: unsupported deployment pool call %T", request), nil
}

// HandleInspect exposes the current pool size.
func (p *deploymentPool[T]) HandleInspect(_ gen.PID, _ ...string) map[string]string {
	return map[string]string{"ergo:pool_size": fmt.Sprintf("%d", p.size)}
}

// ---------------------------------------------------------------------------
// Status aggregation
// ---------------------------------------------------------------------------

// publishStatus reports aggregate worker health to the manager.
func (p *deploymentPool[T]) publishStatus() {
	next := deploymentPoolStatus{
		desiredWorkers: int(p.size),
		workers:        make(map[gen.PID]deploymentWorkerStatus, len(p.workers)),
	}
	failed := false
	restarting := false
	for pid, worker := range p.workers {
		next.workers[pid] = worker
		if worker.availability == runtime.AvailabilityReady {
			next.healthyWorkers++
		}
		switch worker.lifecycle {
		case DeploymentWorkerFailed:
			failed = true
		case DeploymentWorkerRestarting:
			restarting = true
		}
	}
	if failed {
		next.lifecycle = DeploymentPoolFailed
		next.availability = runtime.AvailabilityUnavailable
	} else if next.healthyWorkers > 0 {
		next.lifecycle = DeploymentPoolRunning
		if next.healthyWorkers >= next.desiredWorkers {
			next.availability = runtime.AvailabilityReady
		} else {
			next.availability = runtime.AvailabilityDegraded
		}
	} else if restarting {
		next.lifecycle = DeploymentPoolRestarting
		next.availability = runtime.AvailabilityUnavailable
	} else {
		next.lifecycle = DeploymentPoolStarting
		next.availability = runtime.AvailabilityUnavailable
	}
	_ = p.SendWithPriority(p.Parent(), MessageDeploymentPoolStatusChanged{pool: p.PID(), status: next}, gen.MessagePriorityHigh)
}
