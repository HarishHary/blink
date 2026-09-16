package stage

import (
	"fmt"
	"strconv"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// JobPoolLifecycle describes a job-pool lifecycle state.
type JobPoolLifecycle string

const (
	JobPoolStarting JobPoolLifecycle = "starting"
	JobPoolRunning  JobPoolLifecycle = "running"
	JobPoolStopped  JobPoolLifecycle = "stopped"
)

// JobPoolStatus reports job-pool lifecycle and availability.
type JobPoolStatus struct {
	LastError    error
	Lifecycle    JobPoolLifecycle
	Availability runtime.Availability
}

// jobPool routes jobs round-robin while coordinators manage admission and completion.
type jobPool struct {
	act.Pool
	opts            JobPoolOptions
	lifecycle       JobPoolLifecycle
	err             error
	lastStatus      JobPoolStatus
	lastStatusEpoch int64
	labels          telemetry.Labels
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageJobPoolStarted marks completion of job-pool worker startup.
type MessageJobPoolStarted struct{}

// MessageJobPoolStatusRequest requests a job pool's current status.
type MessageJobPoolStatusRequest struct{}

// MessageJobPoolStatusChanged reports a job pool's current status.
type MessageJobPoolStatusChanged struct {
	StatusEpoch int64
	Status      JobPoolStatus
}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// NewJobPool creates a job pool with a copy of its worker arguments.
func NewJobPool(opts JobPoolOptions) gen.ProcessBehavior {
	opts.WorkerArgs = append([]any(nil), opts.WorkerArgs...)
	return &jobPool{opts: opts, labels: telemetry.NewLabels(opts.Namespace)}
}

// Init validates the pool configuration and schedules startup completion.
func (p *jobPool) Init(...any) (act.PoolOptions, error) {
	if err := validateJobPoolOptions(p.opts); err != nil {
		p.err = err
		return act.PoolOptions{}, err
	}
	info, err := p.Info()
	if err != nil {
		p.err = fmt.Errorf("job pool: inspect mailbox: %w", err)
		return act.PoolOptions{}, p.err
	}
	if info.MailboxSize != p.opts.MailboxSize {
		p.err = fmt.Errorf("job pool: mailbox size is %d, want %d", info.MailboxSize, p.opts.MailboxSize)
		return act.PoolOptions{}, p.err
	}
	p.lifecycle = JobPoolStarting
	p.reconcileStatus()
	// The high-priority self-message reports ready after the pool creates its workers.
	if err := p.SendWithPriority(p.PID(), MessageJobPoolStarted{}, gen.MessagePriorityHigh); err != nil {
		p.err = fmt.Errorf("job pool: schedule startup: %w", err)
		return act.PoolOptions{}, p.err
	}
	return act.PoolOptions{
		PoolSize:          p.opts.Workers,
		WorkerMailboxSize: p.opts.WorkerMailboxSize,
		WorkerFactory:     p.opts.WorkerFactory,
		WorkerArgs:        append([]any(nil), p.opts.WorkerArgs...),
	}, nil
}

// HandleMessage handles pool management while act.Pool forwards normal-priority traffic.
func (p *jobPool) HandleMessage(from gen.PID, message any) error {
	switch message.(type) {
	case MessageJobPoolStatusRequest:
		if from != p.Parent() {
			return nil
		}
		epoch := p.lastStatusEpoch
		p.reconcileStatus()
		if p.lastStatusEpoch == epoch {
			p.propagateStatus(p.lastStatus)
		}
		return nil
	case MessageJobPoolStarted:
		if from != p.PID() || p.lifecycle != JobPoolStarting {
			return nil
		}
		p.lifecycle = JobPoolRunning
		p.reconcileStatus()
		return nil
	}
	return nil
}

// HandleCall returns unsupported calls as responses without terminating the pool.
func (p *jobPool) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("job pool: unsupported call %T", request), nil
}

// Terminate marks the pool stopped, keeping the earliest error it recorded.
func (p *jobPool) Terminate(reason error) {
	p.lifecycle = JobPoolStopped
	p.err = runtime.FirstError(reason, p.err)
	p.reconcileStatus()
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// status returns the pool lifecycle and availability.
func (p *jobPool) status() JobPoolStatus {
	availability := runtime.AvailabilityUnavailable
	if p.lifecycle == JobPoolRunning {
		availability = runtime.AvailabilityReady
		if p.err != nil {
			availability = runtime.AvailabilityDegraded
		}
	}
	return JobPoolStatus{
		Lifecycle:    p.lifecycle,
		Availability: availability,
		LastError:    p.err,
	}
}

// reconcileStatus refreshes gauges on every reconciliation and propagates status only on change.
func (p *jobPool) reconcileStatus() {
	p.publishGauges()
	next := p.status()
	if sameJobPoolStatus(p.lastStatus, next) {
		return
	}
	p.lastStatusEpoch = runtime.NextStatusEpoch(p.lastStatusEpoch)
	p.lastStatus = next
	p.propagateStatus(next)
}

// propagateStatus sends the supplied snapshot without reconciling state or publishing gauges.
func (p *jobPool) propagateStatus(next JobPoolStatus) {
	_ = p.SendWithPriority(p.Parent(), MessageJobPoolStatusChanged{StatusEpoch: p.lastStatusEpoch, Status: next}, gen.MessagePriorityHigh)
}

// HandleInspect returns pool inspection data with lifecycle and availability.
func (p *jobPool) HandleInspect(from gen.PID, item ...string) map[string]string {
	result := p.Pool.HandleInspect(from, item...)
	status := p.status()
	result["job_pool:last_error"] = runtime.ErrorText(status.LastError)
	result["job_pool:lifecycle"] = string(status.Lifecycle)
	result["job_pool:availability"] = string(status.Availability)
	return result
}

// publishGauges publishes the current worker count and availability.
func (p *jobPool) publishGauges() {
	workers := p.opts.Workers
	if inspected := p.Pool.HandleInspect(p.PID()); inspected != nil {
		if value, err := strconv.ParseInt(inspected["ergo:pool_size"], 10, 64); err == nil {
			workers = value
		}
	}
	jobPoolGauges{availability: p.status().Availability, workers: workers}.publish(p.labels, p)
}

// sameJobPoolStatus compares the status fields that trigger publication.
func sameJobPoolStatus(left, right JobPoolStatus) bool {
	return left.Lifecycle == right.Lifecycle &&
		left.Availability == right.Availability &&
		runtime.ErrorText(left.LastError) == runtime.ErrorText(right.LastError)
}
