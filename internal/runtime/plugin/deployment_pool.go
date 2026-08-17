package plugin

import (
	"fmt"
	"maps"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
)

// DeploymentPoolLifecycle remains the catalog-facing lifecycle facade for a
// deployment manager.
type DeploymentPoolLifecycle string

const (
	DeploymentPoolStarting   DeploymentPoolLifecycle = "starting"
	DeploymentPoolRunning    DeploymentPoolLifecycle = "running"
	DeploymentPoolRestarting DeploymentPoolLifecycle = "restarting"
	DeploymentPoolFailed     DeploymentPoolLifecycle = "failed"
	DeploymentPoolDraining   DeploymentPoolLifecycle = "draining"
	DeploymentPoolStopped    DeploymentPoolLifecycle = "stopped"
)

type deploymentPoolState struct {
	pid    gen.PID
	status DeploymentPoolStatus
}

// DeploymentPoolStatus preserves the existing router/catalog status contract.
type DeploymentPoolStatus struct {
	Lifecycle      DeploymentPoolLifecycle
	Availability   runtime.Availability
	HealthyWorkers int
	DesiredWorkers int
	QueueDepth     int
	ActiveCalls    int
	Workers        map[gen.Alias]WorkerMetaStatus
}

func (s DeploymentPoolStatus) clone() DeploymentPoolStatus {
	clone := s
	clone.Workers = make(map[gen.Alias]WorkerMetaStatus, len(s.Workers))
	maps.Copy(clone.Workers, s.Workers)
	return clone
}

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
	for alias, worker := range left.Workers {
		other, ok := right.Workers[alias]
		if !ok || worker.Lifecycle != other.Lifecycle ||
			worker.Availability != other.Availability ||
			worker.Activity != other.Activity ||
			errorText(worker.LastError) != errorText(other.LastError) {
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
}

// --- messages ---

type MessageDeploymentPoolAddWorker struct{}
type MessageDeploymentPoolRemoveWorker struct{}
type MessageDeploymentPoolResized struct {
	pool gen.PID
	size int64
	err  error
}

// --- messages ---

func (p *DeploymentPool[T]) Init(...any) (act.PoolOptions, error) {
	p.options = deploymentPoolOptionsWithDefaults(p.options)
	p.size = p.options.InitialSize
	return act.PoolOptions{
		PoolSize:          p.size,
		WorkerMailboxSize: 1,
		WorkerFactory: func() gen.ProcessBehavior {
			return &DeploymentWorker[T]{
				adapter:    p.adapter,
				options:    p.options.Worker,
				deployment: p.deployment,
				manager:    p.Parent(),
			}
		},
	}, nil
}

func (p *DeploymentPool[T]) HandleMessage(from gen.PID, message any) error {
	if from != p.Parent() {
		return nil
	}

	switch message.(type) {
	case MessageDeploymentPoolAddWorker:
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
		_ = p.SendWithPriority(p.Parent(), MessageDeploymentPoolResized{pool: p.PID(), size: p.size, err: err}, gen.MessagePriorityHigh)

	case MessageDeploymentPoolRemoveWorker:
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
		_ = p.SendWithPriority(p.Parent(), MessageDeploymentPoolResized{pool: p.PID(), size: p.size, err: err}, gen.MessagePriorityHigh)
	}
	return nil
}

func (p *DeploymentPool[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("actorruntime: unsupported deployment pool call %T", request), nil
}

func (p *DeploymentPool[T]) HandleInspect(_ gen.PID, _ ...string) map[string]string {
	return map[string]string{"ergo:pool_size": fmt.Sprintf("%d", p.size)}
}
