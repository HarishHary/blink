package plugin

import (
	"errors"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// ---------------------------------------------------------------------------
// Types & state
// ---------------------------------------------------------------------------

// pluginRuntimeSupervisor is the branch over one plugin runtime: the gateway that admits, and the runtime
// that executes. Rest-for-one with the gateway first is the point of the split — a lost runtime leaves the
// gateway standing to fail its invocations, while a lost gateway takes the runtime with it.
type pluginRuntimeSupervisor[P Artifact, M any] struct {
	act.Supervisor
	namespace            string
	opts                 ApplicationOptions
	adapter              *Adapter[P]
	loader               Loader[M]
	gateway              gatewayActorState
	supervisor           supervisorState
	collectorsRegistered bool
	radarLogged          bool
	labels               telemetry.Labels
	signal               telemetry.Signal
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessageRadarTick drives the branch's periodic radar reconcile.
type MessageRadarTick struct{}

// ---------------------------------------------------------------------------
// Actor lifecycle & handlers
// ---------------------------------------------------------------------------

// newPluginRuntimeSupervisor creates the branch supervisor for one plugin runtime.
func newPluginRuntimeSupervisor[P Artifact, M any](opts ApplicationOptions, adapter *Adapter[P], loader Loader[M]) gen.ProcessBehavior {
	return &pluginRuntimeSupervisor[P, M]{
		namespace: opts.Namespace,
		opts:      opts,
		adapter:   adapter,
		loader:    loader,
		labels:    telemetry.NewLabels(opts.Namespace),
		signal:    newHealthSignal(opts.Namespace),
	}
}

// Init validates the branch and declares its two children in admission order.
func (s *pluginRuntimeSupervisor[P, M]) Init(...any) (act.SupervisorSpec, error) {
	s.opts = runtimeOptionsWithDefaults(s.opts)
	s.namespace = s.opts.Namespace
	if s.namespace == "" || s.adapter == nil || s.loader == nil || isNilLoader(s.loader) {
		return act.SupervisorSpec{}, fmt.Errorf("namespace, adapter, and loader are required")
	}
	if err := requireSubtreeName(s, PluginRuntimeName(s.namespace)); err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("plugin runtime supervisor name: %w", err)
	}
	s.labels = telemetry.NewLabels(s.namespace)
	s.signal = newHealthSignal(s.namespace)
	// Neither child has reported yet, so the namespace starts advertising nothing.
	s.gateway.status = gatewayActorStatus{lifecycle: GatewayStarting, availability: runtime.AvailabilityUnavailable}
	s.supervisor.status = SupervisorStatus{lifecycle: SupervisorStarting, Availability: runtime.AvailabilityUnavailable}
	// A message, not an inline call: radar must not delay the spec.
	if err := s.Send(s.PID(), MessageRadarTick{}); err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("schedule radar tick: %w", err)
	}

	gatewayOpts := s.opts.GatewayOptions
	supervisorOpts := s.opts.SupervisorOptions
	return act.SupervisorSpec{
		Type:                act.SupervisorTypeRestForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy:  act.SupervisorStrategyTransient,
			Intensity: supervisorChildRestartIntensity,
			Period:    supervisorChildRestartPeriod,
		},
		Children: []act.SupervisorChildSpec{
			{
				Name: GatewayName(s.namespace),
				Factory: func() gen.ProcessBehavior {
					return newInvocationGateway[P](s.namespace, gatewayOpts, s.labels)
				},
			},
			{
				Name: SupervisorName(s.namespace),
				Factory: func() gen.ProcessBehavior {
					return newRuntimeSupervisor(s.namespace, supervisorOpts, s.adapter, s.loader)
				},
			},
		},
	}, nil
}

// HandleMessage keeps the branch's radar session and its children's status, both of which outlive either child.
func (s *pluginRuntimeSupervisor[P, M]) HandleMessage(from gen.PID, message any) error {
	defer s.reconcileStatus()
	switch m := message.(type) {
	case MessageRadarTick:
		if from != s.PID() {
			return nil
		}
		s.reconcileRadar()
		if _, err := s.SendAfter(s.PID(), MessageRadarTick{}, telemetry.RadarTickInterval); err != nil {
			return fmt.Errorf("reschedule radar tick: %w", err)
		}

	case MessageGatewayStatusChanged:
		if !s.isChild(GatewayName(s.namespace), from) || m.pid != from || m.statusEpoch <= s.gateway.statusEpoch {
			return nil
		}
		s.gateway.pid = m.pid
		s.gateway.status = m.status
		s.gateway.statusEpoch = m.statusEpoch

	case MessageSupervisorStatusChanged:
		if !s.isChild(SupervisorName(s.namespace), from) || m.pid != from || m.statusEpoch <= s.supervisor.statusEpoch {
			return nil
		}
		s.supervisor.pid = m.pid
		s.supervisor.status = m.status
		s.supervisor.statusEpoch = m.statusEpoch

	case gen.MessageDownProcessID:
		// Forget what a restarted radar lost so the next tick registers it again.
		switch m.ProcessID.Name {
		case telemetry.MetricsProcess:
			s.collectorsRegistered = false
			s.Log().Debug("radar metrics down, re-registering on next tick: namespace=%q", s.namespace)
		case telemetry.HealthProcess:
			s.signal = newHealthSignal(s.namespace)
			s.Log().Debug("radar health down, re-registering on next tick: namespace=%q", s.namespace)
		}
	}
	return nil
}

// HandleChildStart counts a branch child incarnation, the only record that one restarted, and waits for the
// new incarnation's own account of itself rather than trusting the one it replaced.
func (s *pluginRuntimeSupervisor[P, M]) HandleChildStart(name gen.Atom, pid gen.PID) error {
	defer s.reconcileStatus()
	switch name {
	case GatewayName(s.namespace):
		s.labels.Count(s, metricChildStarts, "gateway")
		if s.gateway.pid == pid {
			return nil
		}
		s.gateway.pid = pid
		s.gateway.statusEpoch = 0
		s.gateway.status = gatewayActorStatus{lifecycle: GatewayStarting, availability: runtime.AvailabilityUnavailable}

	case SupervisorName(s.namespace):
		s.labels.Count(s, metricChildStarts, "supervisor")
		if s.supervisor.pid == pid {
			return nil
		}
		s.supervisor.pid = pid
		s.supervisor.statusEpoch = 0
		s.supervisor.status = SupervisorStatus{lifecycle: SupervisorStarting, Availability: runtime.AvailabilityUnavailable}
	}
	return nil
}

// HandleChildTerminate counts a branch child exit and takes readiness down on the spot: waiting for radar to
// time the signal out would advertise a namespace as serving for another minute and a half.
func (s *pluginRuntimeSupervisor[P, M]) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	defer s.reconcileStatus()
	switch name {
	case GatewayName(s.namespace):
		s.labels.Count(s, metricChildTerminations, "gateway", telemetry.TerminationReason(reason))
		if s.gateway.pid != pid {
			return nil
		}
		s.gateway.pid = gen.PID{}
		s.gateway.status.availability = runtime.AvailabilityUnavailable
		s.gateway.status.err = reason

	case SupervisorName(s.namespace):
		s.labels.Count(s, metricChildTerminations, "supervisor", telemetry.TerminationReason(reason))
		if s.supervisor.pid != pid {
			return nil
		}
		s.supervisor.pid = gen.PID{}
		s.supervisor.status.Availability = runtime.AvailabilityUnavailable
		s.supervisor.status.err = reason
	}
	return nil
}

// Terminate advertises the namespace down before the process holding its signal disappears.
func (s *pluginRuntimeSupervisor[P, M]) Terminate(error) {
	s.gateway.status.availability = runtime.AvailabilityUnavailable
	s.supervisor.status.Availability = runtime.AvailabilityUnavailable
	s.reconcileStatus()
}

// isChild reports whether a status came from the child registered under name. Resolved by name rather than by
// the PID a start callback recorded: a child's first status can arrive before that callback runs, and its name
// is registered before it does.
func (s *pluginRuntimeSupervisor[P, M]) isChild(name gen.Atom, from gen.PID) bool {
	pid, err := subtreePID(s.Node(), name)
	return err == nil && pid == from
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// reconcileStatus publishes the branch's roll-up gauge and moves the namespace's readiness with it.
func (s *pluginRuntimeSupervisor[P, M]) reconcileStatus() {
	availability := s.availability()
	pluginRuntimeGauges{readiness: availability}.publish(s.labels, s)
	s.signal.SetReady(s, availability == runtime.AvailabilityReady)
}

// availability is what the namespace can do, not what either half can: nothing runs without a gateway to admit
// it or a runtime to execute it, so the worse of the two decides.
func (s *pluginRuntimeSupervisor[P, M]) availability() runtime.Availability {
	gateway, subtree := s.gateway.status.availability, s.supervisor.status.Availability
	if gateway == runtime.AvailabilityUnavailable || subtree == runtime.AvailabilityUnavailable {
		return runtime.AvailabilityUnavailable
	}
	if gateway != runtime.AvailabilityReady || subtree != runtime.AvailabilityReady {
		return runtime.AvailabilityDegraded
	}
	return runtime.AvailabilityReady
}

// reconcileRadar keeps the branch's radar session: every collector this package publishes, the gateway's
// included, and the namespace's readiness signal. Both are the branch's because the runtime supervisor
// restarts alone under a standing gateway, and neither a registration nor a signal survives that below.
func (s *pluginRuntimeSupervisor[P, M]) reconcileRadar() {
	if !s.collectorsRegistered {
		// Registered through the node: radar deletes a dead registrant's metrics.
		if err := telemetry.Register(s.Node(), runtimeMetrics); err != nil {
			s.radarUnavailableOnce(err)
		} else {
			s.collectorsRegistered = true
			s.watchRadar(telemetry.MetricsProcess)
		}
	}
	if !s.signal.Registered() {
		if !s.watchRadar(telemetry.HealthProcess) {
			return
		}
		if err := s.signal.Register(s); err != nil {
			s.radarUnavailableOnce(err)
			return
		}
	}
	if s.collectorsRegistered {
		s.radarLogged = false
	}
	s.reconcileStatus()
	s.signal.Heartbeat(s)
}

// watchRadar monitors one radar process and reports whether the watch is installed.
func (s *pluginRuntimeSupervisor[P, M]) watchRadar(name gen.Atom) bool {
	if err := s.MonitorProcessID(gen.ProcessID{Name: name, Node: s.Node().Name()}); err != nil && !errors.Is(err, gen.ErrTargetExist) {
		s.Log().Debug("radar monitor unavailable: namespace=%q process=%s error=%v", s.namespace, name, err)
		return false
	}
	return true
}

// radarUnavailableOnce logs only the first failure of an outage.
func (s *pluginRuntimeSupervisor[P, M]) radarUnavailableOnce(err error) {
	if s.radarLogged {
		return
	}
	s.radarLogged = true
	s.Log().Debug("radar telemetry unavailable: namespace=%q error=%v", s.namespace, err)
}

// HandleInspect exposes what each child last reported and the readiness the branch derives from it. Their
// full status lives with them, so this names where to look.
func (s *pluginRuntimeSupervisor[P, M]) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"plugin_runtime:namespace":               s.namespace,
		"plugin_runtime:gateway":                 string(GatewayName(s.namespace)),
		"plugin_runtime:supervisor":              string(SupervisorName(s.namespace)),
		"plugin_runtime:collectors":              fmt.Sprintf("%t", s.collectorsRegistered),
		"plugin_runtime:availability":            string(s.availability()),
		"plugin_runtime:readiness_signal":        s.signal.State(),
		"plugin_runtime:gateway:lifecycle":       string(s.gateway.status.lifecycle),
		"plugin_runtime:gateway:availability":    string(s.gateway.status.availability),
		"plugin_runtime:gateway:err":             runtime.ErrorText(s.gateway.status.err),
		"plugin_runtime:supervisor:lifecycle":    string(s.supervisor.status.lifecycle),
		"plugin_runtime:supervisor:availability": string(s.supervisor.status.Availability),
		"plugin_runtime:supervisor:err":          runtime.ErrorText(s.supervisor.status.err),
	}
}
