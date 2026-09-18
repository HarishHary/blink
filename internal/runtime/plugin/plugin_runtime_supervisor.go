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
	namespace string
	opts      ApplicationOptions
	adapter   *Adapter[P]
	loader    Loader[M]
	labels    telemetry.Labels
}

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

	gatewayOpts := s.opts.GatewayOptions
	supervisorOpts := s.opts.SupervisorOptions
	return act.SupervisorSpec{
		Type:                act.SupervisorTypeRestForOne,
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

// HandleInspect names the branch's children, since their own status lives with them.
func (s *pluginRuntimeSupervisor[P, M]) HandleInspect(gen.PID, ...string) map[string]string {
	return map[string]string{
		"plugin_runtime:namespace":  s.namespace,
		"plugin_runtime:gateway":    string(GatewayName(s.namespace)),
		"plugin_runtime:supervisor": string(SupervisorName(s.namespace)),
	}
}
