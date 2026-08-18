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
	status        DeploymentPoolStatus
	restart       *runtime.ScheduledBackoff
	resizePending bool
	expectedStop  bool
	recovering    bool
}

// DeploymentPoolStatus preserves the existing router/catalog status contract.
type DeploymentPoolStatus struct {
	Lifecycle      DeploymentPoolLifecycle
	Availability   runtime.Availability
	HealthyWorkers int
	DesiredWorkers int
	QueueDepth     int
	ActiveCalls    int
	Workers        map[gen.PID]DeploymentWorkerStatus
}

// clone copies a pool status and its worker map.
func (s DeploymentPoolStatus) clone() DeploymentPoolStatus {
	clone := s
	clone.Workers = make(map[gen.PID]DeploymentWorkerStatus, len(s.Workers))
	maps.Copy(clone.Workers, s.Workers)
	return clone
}

// sameDeploymentPoolStatus compares pool status snapshots.
func sameDeploymentPoolStatus(left, right DeploymentPoolStatus) bool {
	if left.Lifecycle != right.Lifecycle ||
		left.Availability != right.Availability ||
		left.HealthyWorkers != right.HealthyWorkers ||
		left.DesiredWorkers != right.DesiredWorkers ||
		left.QueueDepth != right.QueueDepth ||
		left.ActiveCalls != right.ActiveCalls ||
		len(left.Workers) != len(right.Workers) {
		return false
	}
	for pid, worker := range left.Workers {
		other, ok := right.Workers[pid]
		if !ok || !sameDeploymentWorkerStatus(worker, other) {
			return false
		}
	}
	return true
}

// DeploymentPool places deployment workers and forwards normal-priority traffic.
type DeploymentPool[T Syncable] struct {
	act.Pool
	adapter    *Adapter[T]
	options    DeploymentPoolOptions
	deployment Deployment
	size       int64
	workers    map[gen.PID]deploymentWorkerState
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
	status DeploymentPoolStatus
}

// ---------------------------------------------------------------------------
// Actor lifecycle
// ---------------------------------------------------------------------------

// Init configures the pool and creates its worker factory.
func (p *DeploymentPool[T]) Init(...any) (act.PoolOptions, error) {
	p.options = deploymentPoolOptionsWithDefaults(p.options)
	p.size = p.options.InitialSize
	p.workers = make(map[gen.PID]deploymentWorkerState)
	p.publishStatus()
	return act.PoolOptions{
		PoolSize:          p.size,
		WorkerMailboxSize: 1,
		WorkerFactory: func() gen.ProcessBehavior {
			return &DeploymentWorker[T]{
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
func (p *DeploymentPool[T]) HandleMessage(from gen.PID, message any) error {
	switch msg := message.(type) {
	case MessageDeploymentWorkerStatusChanged:
		if from != msg.worker {
			return nil
		}
		p.workers[msg.worker] = deploymentWorkerState{status: msg.status}
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
func (p *DeploymentPool[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("actorruntime: unsupported deployment pool call %T", request), nil
}

// HandleInspect exposes the current pool size.
func (p *DeploymentPool[T]) HandleInspect(_ gen.PID, _ ...string) map[string]string {
	return map[string]string{"ergo:pool_size": fmt.Sprintf("%d", p.size)}
}

// ---------------------------------------------------------------------------
// Status aggregation
// ---------------------------------------------------------------------------

// publishStatus reports aggregate worker health to the manager.
func (p *DeploymentPool[T]) publishStatus() {
	next := DeploymentPoolStatus{
		DesiredWorkers: int(p.size),
		Workers:        make(map[gen.PID]DeploymentWorkerStatus, len(p.workers)),
	}
	failed := false
	restarting := false
	for pid, worker := range p.workers {
		next.Workers[pid] = worker.status
		if worker.status.Availability == runtime.AvailabilityReady {
			next.HealthyWorkers++
		}
		switch worker.status.Lifecycle {
		case DeploymentWorkerFailed:
			failed = true
		case DeploymentWorkerRestarting:
			restarting = true
		}
	}
	if failed {
		next.Lifecycle = DeploymentPoolFailed
		next.Availability = runtime.AvailabilityUnavailable
	} else if next.HealthyWorkers > 0 {
		next.Lifecycle = DeploymentPoolRunning
		if next.HealthyWorkers >= next.DesiredWorkers {
			next.Availability = runtime.AvailabilityReady
		} else {
			next.Availability = runtime.AvailabilityDegraded
		}
	} else if restarting {
		next.Lifecycle = DeploymentPoolRestarting
		next.Availability = runtime.AvailabilityUnavailable
	} else {
		next.Lifecycle = DeploymentPoolStarting
		next.Availability = runtime.AvailabilityUnavailable
	}
	_ = p.SendWithPriority(p.Parent(), MessageDeploymentPoolStatusChanged{pool: p.PID(), status: next}, gen.MessagePriorityHigh)
}
