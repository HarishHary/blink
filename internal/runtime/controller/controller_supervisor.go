package controller

import (
	"errors"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

const (
	actorRestartIntensity uint16 = 5
	actorRestartPeriod    uint16 = 10
)

type SupervisorLifecycle string

const (
	SupervisorStarting SupervisorLifecycle = "starting"
	SupervisorRunning  SupervisorLifecycle = "running"
	SupervisorDraining SupervisorLifecycle = "draining"
	SupervisorStopping SupervisorLifecycle = "stopping"
)

type actorState struct {
	pid            gen.PID
	status         actorStatus
	activationSent bool
}

// writerIOFence tracks a writer's owner and proof that its I/O has finished.
type writerIOFence struct {
	owner      gen.PID
	completion *runtime.IOBarrier
}

type supervisor[T plugin.Artifact] struct {
	act.Supervisor
	opts                 SupervisorOptions
	namespace            string
	loader               plugin.Loader[T]
	database             backends.Database
	barrier              *runtime.IOBarrier
	lifecycle            SupervisorLifecycle
	actor                actorState
	writerFences         map[gen.Alias]writerIOFence
	labels               telemetry.Labels
	signal               telemetry.Signal
	collectorsRegistered bool
	radarLogged          bool
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

type MessageActorStatusChanged struct {
	status actorStatus
}

// MessageRadarTick drives the supervisor's periodic radar reconcile.
// It stays high priority because it also releases completed I/O fences and unblocks replacements.
type MessageRadarTick struct{}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// newSupervisor constructs the controller supervisor with the namespace its application configured,
// normalized options, and its typed loader.
func newSupervisor[T plugin.Artifact](namespace string, opts SupervisorOptions, loader plugin.Loader[T], database backends.Database, barrier *runtime.IOBarrier) gen.ProcessBehavior {
	return &supervisor[T]{
		opts:      supervisorOptionsWithDefaults(opts),
		namespace: namespace,
		loader:    loader,
		database:  database,
		barrier:   barrier,
		labels:    telemetry.NewLabels(namespace),
		signal:    newHealthSignal(namespace),
	}
}

// Init configures the supervised controller actor and opens this namespace's radar session.
func (s *supervisor[T]) Init(...any) (act.SupervisorSpec, error) {
	// Namespace is required: every process name in this subtree, and every metric label, comes from it.
	if s.namespace == "" {
		return act.SupervisorSpec{}, fmt.Errorf("controller supervisor: namespace is required")
	}
	s.actor.status = actorStatus{
		Lifecycle:    ActorStarting,
		Availability: runtime.AvailabilityUnavailable,
	}
	s.lifecycle = SupervisorStarting
	s.writerFences = make(map[gen.Alias]writerIOFence)
	// A message, not an inline call: the signal must exist before any probe reads it, but radar must
	// not delay the spec.
	if err := s.SendWithPriority(s.PID(), MessageRadarTick{}, gen.MessagePriorityHigh); err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("controller supervisor: schedule radar tick: %w", err)
	}
	return act.SupervisorSpec{
		Type:                act.SupervisorTypeOneForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy:  act.SupervisorStrategyTransient,
			Intensity: actorRestartIntensity,
			Period:    actorRestartPeriod,
		},
		Children: []act.SupervisorChildSpec{{
			Name: ActorName(s.namespace),
			Options: gen.ProcessOptions{
				PreserveMailbox: true,
			},
			Factory: func() gen.ProcessBehavior {
				return newActor(s.opts.ActorOptions, s.loader, s.labels, s.database, s.barrier)
			},
		}},
	}, nil
}

// HandleMessage coordinates controller lifecycle and writer I/O fences.
func (s *supervisor[T]) HandleMessage(from gen.PID, message any) error {
	defer s.publishState()
	switch m := message.(type) {
	case MessageRadarTick:
		if from != s.PID() {
			return nil
		}
		if err := s.pollIOCompletions(); err != nil {
			return err
		}
		s.reconcileRadar()
		if s.lifecycle != SupervisorStopping {
			if _, err := s.SendWithPriorityAfter(s.PID(), MessageRadarTick{}, gen.MessagePriorityHigh, telemetry.RadarTickInterval); err != nil {
				return fmt.Errorf("reschedule radar tick: %w", err)
			}
		}
		return nil
	case gen.MessageDownProcessID:
		// Drop what a restarted radar process lost; the signal is rebuilt, not flagged, because radar
		// marks a fresh registration up.
		switch m.ProcessID.Name {
		case telemetry.MetricsProcess:
			s.collectorsRegistered = false
		case telemetry.HealthProcess:
			s.signal = newHealthSignal(s.namespace)
		default:
			return nil
		}
		s.Log().Debug("radar process down, re-registering on next tick: namespace=%q process=%s", s.namespace, m.ProcessID.Name)
		return nil
	case plugin.MessageStop:
		if s.lifecycle != SupervisorRunning {
			return nil
		}
		s.lifecycle = SupervisorDraining
		s.Log().Info("controller supervisor draining: name=%s child=%s writer_fences=%d", s.Name(), s.actor.pid, len(s.writerFences))
		if err := s.sendDrain(); err != nil {
			return err
		}
		return s.advanceShutdown()
	case MessageActorStatusChanged:
		if s.actor.pid != from {
			return nil
		}
		previous := s.actor.status.Lifecycle
		s.actor.status = m.status
		if previous != m.status.Lifecycle {
			s.Log().Debug("controller child lifecycle changed: name=%s child=%s lifecycle=%s", s.Name(), from, m.status.Lifecycle)
		}
		if s.lifecycle == SupervisorDraining {
			return s.advanceShutdown()
		}
	case MessageSnapshotWriterIOStarted:
		if s.actor.pid != from || m.Alias == (gen.Alias{}) || m.completion == nil {
			return nil
		}
		if _, exists := s.writerFences[m.Alias]; exists {
			return nil
		}
		if s.writerFences == nil {
			s.writerFences = make(map[gen.Alias]writerIOFence)
		}
		s.writerFences[m.Alias] = writerIOFence{owner: from, completion: m.completion}
		s.Log().Debug("snapshot writer I/O fence registered: name=%s child=%s alias=%s active=%d", s.Name(), from, m.Alias, len(s.writerFences))
	case MessageSnapshotWriterIOStopped:
		return s.completeIOFence(from, m.Alias)
	}
	return nil
}

// HandleChildStart tracks a newly started controller actor.
func (s *supervisor[T]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	if name != ActorName(s.namespace) || s.actor.pid != (gen.PID{}) {
		return nil
	}
	defer s.publishState()
	s.labels.Count(s, metricChildStarts)
	s.actor = actorState{
		pid: pid,
		status: actorStatus{
			Lifecycle:    ActorStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
	}
	s.Log().Info("controller child started: name=%s child=%s", s.Name(), pid)
	return s.reconcileActor()
}

// HandleChildTerminate handles the tracked controller actor exiting.
func (s *supervisor[T]) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	if name != ActorName(s.namespace) || s.actor.pid != pid {
		return nil
	}
	defer s.publishState()
	s.labels.Count(s, metricChildTerminations, telemetry.TerminationReason(reason))
	s.actor.pid = gen.PID{}
	if reason == gen.TerminateReasonNormal || reason == gen.TerminateReasonShutdown {
		switch s.lifecycle {
		case SupervisorDraining, SupervisorStopping:
			s.Log().Info("controller child stopped: name=%s child=%s reason=%v", s.Name(), pid, reason)
			return s.advanceShutdown()
		default:
			s.Log().Error("controller child stopped unexpectedly: name=%s child=%s reason=%v", s.Name(), pid, reason)
			return fmt.Errorf("controller supervisor: child %s (%s) exited unexpectedly: %w", name, pid, reason)
		}
	}
	s.Log().Error("controller child failed: name=%s child=%s reason=%v", s.Name(), pid, reason)
	return nil
}

// HandleCall rejects unsupported supervisor calls.
func (s *supervisor[T]) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return fmt.Errorf("controller supervisor: unsupported call %T", request), nil
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// completeIOFence releases a writer fence only after its I/O completion is proven.
func (s *supervisor[T]) completeIOFence(from gen.PID, alias gen.Alias) error {
	fence, ok := s.writerFences[alias]
	if !ok || fence.owner != from || !fence.completion.Quiesced() {
		return nil
	}
	if s.actor.pid == from {
		if err := s.SendWithPriority(from, MessageSnapshotWriterIOStopped{Alias: alias}, gen.MessagePriorityHigh); err != nil && !stalePIDSendFailure(err) {
			s.Log().Error("snapshot writer I/O completion forwarding failed: name=%s child=%s alias=%s error=%v", s.Name(), from, alias, err)
			//argus:allow A2012 forwarding failure is fatal because losing this completion blocks writer replacement and drain
			return fmt.Errorf("forward snapshot writer I/O completion to %s: %w", from, err)
		}
	}
	delete(s.writerFences, alias)
	s.Log().Debug("snapshot writer I/O fence released: name=%s child=%s alias=%s active=%d", s.Name(), from, alias, len(s.writerFences))
	return s.reconcileActor()
}

// pollIOCompletions recovers completed writer fences when an I/O-stopped notification was lost.
func (s *supervisor[T]) pollIOCompletions() error {
	for alias, fence := range s.writerFences {
		if fence.completion.Quiesced() {
			if err := s.completeIOFence(fence.owner, alias); err != nil {
				return err
			}
		}
	}
	return nil
}

// advanceShutdown stops the actor after draining writer I/O.
func (s *supervisor[T]) advanceShutdown() error {
	if s.lifecycle == SupervisorDraining {
		if s.actor.pid != (gen.PID{}) && s.actor.status.Lifecycle != ActorDrained {
			return nil
		}
		if len(s.writerFences) != 0 {
			return nil
		}
		s.lifecycle = SupervisorStopping
		s.Log().Info("controller supervisor stopping: name=%s", s.Name())
		if err := s.sendStop(); err != nil {
			return err
		}
	}
	if s.lifecycle == SupervisorStopping && s.actor.pid == (gen.PID{}) && len(s.writerFences) == 0 {
		s.Log().Info("controller supervisor stopped: name=%s", s.Name())
		return gen.TerminateReasonNormal
	}
	return nil
}

// reconcileActor activates or stops the current controller actor.
func (s *supervisor[T]) reconcileActor() error {
	if s.actor.pid == (gen.PID{}) {
		return s.advanceShutdown()
	}
	if s.lifecycle == SupervisorStopping {
		return s.sendStop()
	}
	if s.lifecycle == SupervisorDraining {
		if len(s.writerFences) != 0 {
			return nil
		}
		s.lifecycle = SupervisorStopping
		return s.sendStop()
	}
	if len(s.writerFences) != 0 || s.actor.activationSent {
		if len(s.writerFences) != 0 {
			s.Log().Debug("controller activation waiting for writer I/O: name=%s child=%s active=%d", s.Name(), s.actor.pid, len(s.writerFences))
		}
		return nil
	}
	err := s.SendWithPriority(s.actor.pid, MessageActorActivate{}, gen.MessagePriorityHigh)
	if err != nil && !stalePIDSendFailure(err) {
		s.Log().Error("controller activation failed: name=%s child=%s error=%v", s.Name(), s.actor.pid, err)
		return fmt.Errorf("activate controller %s: %w", s.actor.pid, err)
	}
	s.actor.activationSent = true
	s.lifecycle = SupervisorRunning
	if err == nil {
		s.Log().Info("controller activated: name=%s child=%s", s.Name(), s.actor.pid)
	}
	return nil
}

// sendDrain asks the controller actor to drain.
func (s *supervisor[T]) sendDrain() error {
	if s.actor.pid == (gen.PID{}) {
		return nil
	}
	err := s.SendWithPriority(s.actor.pid, plugin.MessageDrain{}, gen.MessagePriorityHigh)
	if err != nil && !stalePIDSendFailure(err) {
		s.Log().Error("controller drain request failed: name=%s child=%s error=%v", s.Name(), s.actor.pid, err)
		return fmt.Errorf("drain controller %s: %w", s.actor.pid, err)
	}
	if err == nil {
		s.Log().Debug("controller drain requested: name=%s child=%s", s.Name(), s.actor.pid)
	}
	return nil
}

// sendStop asks the controller actor to stop.
func (s *supervisor[T]) sendStop() error {
	if s.actor.pid == (gen.PID{}) {
		return nil
	}
	err := s.SendWithPriority(s.actor.pid, plugin.MessageStop{}, gen.MessagePriorityHigh)
	if err != nil && !stalePIDSendFailure(err) {
		s.Log().Error("controller stop request failed: name=%s child=%s error=%v", s.Name(), s.actor.pid, err)
		return fmt.Errorf("stop controller %s: %w", s.actor.pid, err)
	}
	if err == nil {
		s.Log().Debug("controller stop requested: name=%s child=%s", s.Name(), s.actor.pid)
	}
	return nil
}

// stalePIDSendFailure reports sends lost to an exiting process.
func stalePIDSendFailure(err error) bool {
	return errors.Is(err, gen.ErrProcessUnknown) || errors.Is(err, gen.ErrProcessTerminated)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// publishState reports supervisor lifecycle and writer fences, then moves the readiness signal.
func (s *supervisor[T]) publishState() {
	s.labels.Set(s, metricSupervisorLifecycle, supervisorLifecycleValue(s.lifecycle))
	s.labels.Set(s, metricWriterFences, float64(len(s.writerFences)))
	s.signal.SetReady(s, s.readyToServe())
}

// readyToServe reports whether this namespace can answer snapshot requests: running, live child, ready child.
func (s *supervisor[T]) readyToServe() bool {
	return s.lifecycle == SupervisorRunning &&
		s.actor.pid != (gen.PID{}) &&
		s.actor.status.Availability == runtime.AvailabilityReady
}

// reconcileRadar registers whatever radar is still missing, then heartbeats the readiness signal.
func (s *supervisor[T]) reconcileRadar() {
	if !s.collectorsRegistered {
		if err := telemetry.Register(s.Node(), controllerMetrics); err != nil {
			s.radarUnavailableOnce(err)
			return
		}
		s.collectorsRegistered = true
		s.watchRadar(telemetry.MetricsProcess)
	}
	if !s.signal.Registered() {
		if err := s.signal.Register(s); err != nil {
			s.radarUnavailableOnce(err)
			return
		}
		s.watchRadar(telemetry.HealthProcess)
	}
	s.radarLogged = false
	s.signal.SetReady(s, s.readyToServe())
	s.signal.Heartbeat(s)
}

// watchRadar monitors one radar process so a restart that drops these registrations announces itself.
func (s *supervisor[T]) watchRadar(name gen.Atom) {
	if err := s.MonitorProcessID(gen.ProcessID{Name: name, Node: s.Node().Name()}); err != nil {
		s.Log().Debug("radar monitor unavailable: namespace=%q process=%s error=%v", s.namespace, name, err)
	}
}

// radarUnavailableOnce logs only the first failure of an outage.
func (s *supervisor[T]) radarUnavailableOnce(err error) {
	if s.radarLogged {
		return
	}
	s.radarLogged = true
	s.Log().Debug("radar telemetry unavailable: namespace=%q error=%v", s.namespace, err)
}

// HandleInspect exposes supervisor lifecycle, the child's identity and status, and pending writer fences.
func (s *supervisor[T]) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"supervisor:lifecycle":          string(s.lifecycle),
		"supervisor:child":              fmt.Sprintf("%s", s.actor.pid),
		"supervisor:child_lifecycle":    string(s.actor.status.Lifecycle),
		"supervisor:child_availability": string(s.actor.status.Availability),
		"supervisor:child_generation":   fmt.Sprintf("%d", s.actor.status.Generation),
		"supervisor:writer_fences":      fmt.Sprintf("%d", len(s.writerFences)),
		"supervisor:readiness_signal":   s.signal.State(),
	}
}
