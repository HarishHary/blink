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
// Messages
// ---------------------------------------------------------------------------

// MessageJobPoolStarted marks completion of job-pool worker startup.
type MessageJobPoolStarted struct{}

// MessageJobPoolMetricsTick requests job-pool metric publication.
type MessageJobPoolMetricsTick struct{}

// MessageJobPoolStatusRequest requests a job pool's current status.
type MessageJobPoolStatusRequest struct{}

// MessageJobPoolStatusChanged reports a job pool's current status.
type MessageJobPoolStatusChanged struct{ Status JobPoolStatus }

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
	Lifecycle    JobPoolLifecycle
	Availability runtime.Availability
}

// jobPool routes jobs round-robin while coordinators manage admission and completion.
type jobPool struct {
	act.Pool
	opts       JobPoolOptions
	lifecycle  JobPoolLifecycle
	lastStatus JobPoolStatus
	labels     telemetry.Labels
}

// ---------------------------------------------------------------------------
// Pool lifecycle & handlers
// ---------------------------------------------------------------------------

// NewJobPool creates a job pool with a copy of its worker arguments.
func NewJobPool(opts JobPoolOptions) gen.ProcessBehavior {
	opts.WorkerArgs = append([]any(nil), opts.WorkerArgs...)
	return &jobPool{opts: opts, labels: telemetry.NewLabels(opts.Namespace)}
}

// Init validates the pool configuration and schedules startup completion.
func (p *jobPool) Init(...any) (act.PoolOptions, error) {
	if err := validateJobPoolOptions(p.opts); err != nil {
		return act.PoolOptions{}, err
	}
	info, err := p.Info()
	if err != nil {
		return act.PoolOptions{}, fmt.Errorf("job pool: inspect mailbox: %w", err)
	}
	if info.MailboxSize != p.opts.MailboxSize {
		return act.PoolOptions{}, fmt.Errorf("job pool: mailbox size is %d, want %d", info.MailboxSize, p.opts.MailboxSize)
	}
	p.lifecycle = JobPoolStarting
	p.publishGauges()
	p.reconcileStatus()
	// The high-priority self-message reports ready after the pool creates its workers.
	if err := p.SendWithPriority(p.PID(), MessageJobPoolStarted{}, gen.MessagePriorityHigh); err != nil {
		return act.PoolOptions{}, fmt.Errorf("job pool: schedule startup: %w", err)
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
		_ = p.SendWithPriority(from, MessageJobPoolStatusChanged{Status: p.status()}, gen.MessagePriorityHigh)
		return nil
	case MessageJobPoolStarted:
		if from != p.PID() || p.lifecycle != JobPoolStarting {
			return nil
		}
		p.lifecycle = JobPoolRunning
		p.publishGauges()
		p.reconcileStatus()
		if _, err := p.SendWithPriorityAfter(p.PID(), MessageJobPoolMetricsTick{}, gen.MessagePriorityHigh, telemetry.RadarTickInterval); err != nil {
			return fmt.Errorf("job pool: schedule initial metrics tick: %w", err)
		}
		return nil
	case MessageJobPoolMetricsTick:
		if from != p.PID() || p.lifecycle != JobPoolRunning {
			return nil
		}
		p.publishGauges()
		if _, err := p.SendWithPriorityAfter(p.PID(), MessageJobPoolMetricsTick{}, gen.MessagePriorityHigh, telemetry.RadarTickInterval); err != nil {
			return fmt.Errorf("job pool: reschedule metrics tick: %w", err)
		}
		return nil
	}
	return nil
}

// HandleCall returns unsupported calls as responses without terminating the pool.
func (p *jobPool) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("job pool: unsupported call %T", request), nil
}

// Terminate marks the pool stopped and publishes its final status.
func (p *jobPool) Terminate(error) {
	p.lifecycle = JobPoolStopped
	p.publishGauges()
	p.reconcileStatus()
}

// ---------------------------------------------------------------------------
// Status & inspection
// ---------------------------------------------------------------------------

// status returns the pool lifecycle and availability.
func (p *jobPool) status() JobPoolStatus {
	availability := runtime.AvailabilityUnavailable
	if p.lifecycle == JobPoolRunning {
		availability = runtime.AvailabilityReady
	}
	return JobPoolStatus{Lifecycle: p.lifecycle, Availability: availability}
}

// reconcileStatus publishes a status change to the parent.
func (p *jobPool) reconcileStatus() {
	next := p.status()
	if next == p.lastStatus {
		return
	}
	p.lastStatus = next
	_ = p.SendWithPriority(p.Parent(), MessageJobPoolStatusChanged{Status: next}, gen.MessagePriorityHigh)
}

// HandleInspect returns pool inspection data with lifecycle and availability.
func (p *jobPool) HandleInspect(from gen.PID, item ...string) map[string]string {
	result := p.Pool.HandleInspect(from, item...)
	status := p.status()
	result["job_pool:lifecycle"] = string(status.Lifecycle)
	result["job_pool:availability"] = string(status.Availability)
	return result
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

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
