package plugin

import (
	"errors"
	"fmt"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
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
	labels               telemetry.Labels
	collectorsRegistered bool
	radarLogged          bool
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// MessagePluginRuntimeRadarTick drives the branch's periodic collector registration.
type MessagePluginRuntimeRadarTick struct{}

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
	// A message, not an inline call: radar must not delay the spec.
	if err := s.Send(s.PID(), MessagePluginRuntimeRadarTick{}); err != nil {
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

// HandleMessage keeps the branch's radar session, which outlives either child.
func (s *pluginRuntimeSupervisor[P, M]) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessagePluginRuntimeRadarTick:
		if from != s.PID() {
			return nil
		}
		s.reconcileRadar()
		if _, err := s.SendAfter(s.PID(), MessagePluginRuntimeRadarTick{}, telemetry.RadarTickInterval); err != nil {
			return fmt.Errorf("reschedule radar tick: %w", err)
		}

	case gen.MessageDownProcessID:
		// Forget what a restarted radar lost so the next tick registers it again.
		if m.ProcessID.Name != telemetry.MetricsProcess {
			return nil
		}
		s.collectorsRegistered = false
		s.Log().Debug("radar metrics down, re-registering on next tick: namespace=%q", s.namespace)
	}
	return nil
}

// HandleChildStart counts a branch child incarnation, the only record that one restarted.
func (s *pluginRuntimeSupervisor[P, M]) HandleChildStart(name gen.Atom, _ gen.PID) error {
	if child, ok := s.childLabel(name); ok {
		s.labels.Count(s, metricChildStarts, child)
	}
	return nil
}

// HandleChildTerminate counts a branch child exit, separating a failure from a shutdown.
func (s *pluginRuntimeSupervisor[P, M]) HandleChildTerminate(name gen.Atom, _ gen.PID, reason error) error {
	if child, ok := s.childLabel(name); ok {
		s.labels.Count(s, metricChildTerminations, child, telemetry.TerminationReason(reason))
	}
	return nil
}

// childLabel names one of the branch's two children for the metrics that count them.
func (s *pluginRuntimeSupervisor[P, M]) childLabel(name gen.Atom) (string, bool) {
	switch name {
	case GatewayName(s.namespace):
		return "gateway", true
	case SupervisorName(s.namespace):
		return "supervisor", true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// reconcileRadar registers every collector this package publishes, the gateway's included. The branch owns
// it because the runtime supervisor restarts alone, and a registration nothing renews is one radar loses.
func (s *pluginRuntimeSupervisor[P, M]) reconcileRadar() {
	if s.collectorsRegistered {
		return
	}
	// Registered through the node: radar deletes a dead registrant's metrics.
	if err := telemetry.Register(s.Node(), runtimeMetrics); err != nil {
		s.radarUnavailableOnce(err)
		return
	}
	s.collectorsRegistered = true
	s.radarLogged = false
	if err := s.MonitorProcessID(gen.ProcessID{Name: telemetry.MetricsProcess, Node: s.Node().Name()}); err != nil && !errors.Is(err, gen.ErrTargetExist) {
		s.Log().Debug("radar monitor unavailable: namespace=%q error=%v", s.namespace, err)
	}
}

// radarUnavailableOnce logs only the first failure of an outage.
func (s *pluginRuntimeSupervisor[P, M]) radarUnavailableOnce(err error) {
	if s.radarLogged {
		return
	}
	s.radarLogged = true
	s.Log().Debug("radar telemetry unavailable: namespace=%q error=%v", s.namespace, err)
}

// HandleInspect names the branch's children, since their own status lives with them.
func (s *pluginRuntimeSupervisor[P, M]) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"plugin_runtime:namespace":  s.namespace,
		"plugin_runtime:gateway":    string(GatewayName(s.namespace)),
		"plugin_runtime:supervisor": string(SupervisorName(s.namespace)),
		"plugin_runtime:collectors": fmt.Sprintf("%t", s.collectorsRegistered),
	}
}
