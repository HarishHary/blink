package matcher

import (
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
)

// Application hosts the matcher runtime and rule snapshot supervisor.
type Application struct {
	*matchers.Application
	ruleOpts snapshot.SupervisorOptions
}

// NewApplication builds the matcher runtime and rule snapshot used by Service.
func NewApplication(opts plugin.ApplicationOptions, ruleOpts snapshot.SupervisorOptions, logger *logger.Logger) *Application {
	return &Application{Application: matchers.NewApplication(opts, logger), ruleOpts: ruleOpts}
}

// Load adds the rule snapshot supervisor to the matcher application's spec.
func (a *Application) Load(...any) (gen.ApplicationSpec, error) {
	spec, err := a.Application.Load()
	if err != nil {
		return gen.ApplicationSpec{}, err
	}
	spec.Group = append(spec.Group, gen.ApplicationMemberSpec{
		Factory: func() gen.ProcessBehavior {
			return snapshot.NewSupervisor(a.ruleOpts, rules.Loader{})
		},
	})
	spec.Map["rule_snapshot"] = snapshot.SupervisorName(a.ruleOpts.Namespace)
	return spec, nil
}
