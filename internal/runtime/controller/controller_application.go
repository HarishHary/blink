package controller

import (
	"context"
	"database/sql"
	"fmt"

	"ergo.services/ergo/app"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/runtime/plugin"
	"github.com/harishhary/blink/internal/runtime/snapshot"
	"github.com/harishhary/blink/internal/runtime/telemetry"
)

// Application owns the resources for one plugin-type controller application.
type Application[T plugin.Artifact] struct {
	app.Application
	opts     ApplicationOptions
	loader   plugin.Loader[T]
	database *sql.DB
	barrier  *writerIOBarrier
	stopped  chan error
	labels   telemetry.Labels
}

// NewApplication creates an unloaded application for one plugin type, with the loader its actor
// parses artifacts through.
func NewApplication[T plugin.Artifact](opts ApplicationOptions, loader plugin.Loader[T]) *Application[T] {
	normalized := applicationOptionsWithDefaults(opts)
	return &Application[T]{
		opts:    normalized,
		loader:  loader,
		barrier: newWriterIOBarrier(),
		stopped: make(chan error, 1),
		labels:  telemetry.NewLabels(normalized.Namespace),
	}
}

// Name returns the application name derived from this controller's namespace.
func (a *Application[T]) Name() gen.Atom { return ApplicationName(a.opts.Namespace) }

// SupervisorName returns the registered root supervisor name, derived the same way.
func (a *Application[T]) SupervisorName() gen.Atom { return SupervisorName(a.opts.Namespace) }

// Stopped reports the application callback without blocking the Ergo runtime.
func (a *Application[T]) Stopped() <-chan error { return a.stopped }

// Seal prevents new writer I/O from starting for this application attempt.
func (a *Application[T]) Seal() { a.barrier.Seal() }

// WaitQuiesced waits for writer I/O accepted before Seal to finish.
func (a *Application[T]) WaitQuiesced(ctx context.Context) error {
	return a.barrier.WaitQuiesced(ctx)
}

// Load opens the application-owned resources and describes its one supervisor.
func (a *Application[T]) Load(_ ...any) (gen.ApplicationSpec, error) {
	a.Log().Debug("controller application loading: name=%s namespace=%q", a.Name(), a.opts.Namespace)
	a.registerMetrics()
	// Namespace is required: every process name in this subtree, and every metric label, comes from it.
	if a.opts.Namespace == "" {
		err := fmt.Errorf("controller application: namespace is required")
		a.labels.Count(a.Node(), metricApplicationLoads, "invalid")
		a.Log().Error("controller application configuration invalid: name=%s namespace=%q error=%v", a.Name(), a.opts.Namespace, err)
		return gen.ApplicationSpec{}, err
	}

	database, err := backends.OpenSQLite(a.opts.DatabaseDSN)
	if err != nil {
		a.labels.Count(a.Node(), metricApplicationLoads, "database")
		a.Log().Error("controller application database open failed: name=%s namespace=%q error=%v", a.Name(), a.opts.Namespace, err)
		return gen.ApplicationSpec{}, fmt.Errorf("open %s controller database: %w", a.opts.Namespace, err)
	}
	store, err := backends.NewSQLite(database, a.opts.Namespace)
	if err != nil {
		_ = database.Close()
		a.labels.Count(a.Node(), metricApplicationLoads, "database")
		a.Log().Error("controller application database initialization failed: name=%s namespace=%q error=%v", a.Name(), a.opts.Namespace, err)
		return gen.ApplicationSpec{}, fmt.Errorf("initialize %s controller database: %w", a.opts.Namespace, err)
	}

	a.database = database
	supervisorOpts := a.opts.SupervisorOptions
	a.labels.Count(a.Node(), metricApplicationLoads, "ok")
	a.labels.Set(a.Node(), metricApplicationState, 1)
	a.Log().Info("controller application loaded: name=%s namespace=%q supervisor=%s", a.Name(), a.opts.Namespace, a.SupervisorName())

	return gen.ApplicationSpec{
		Name:        a.Name(),
		Description: fmt.Sprintf("Blink %s controller", a.opts.Namespace),
		Mode:        gen.ApplicationModePermanent,
		Network:     gen.ApplicationNetwork{RegisterTypes: snapshot.NetworkTypes()},
		Group: []gen.ApplicationMemberSpec{{
			Name: a.SupervisorName(),
			Factory: func() gen.ProcessBehavior {
				return newSupervisor(a.opts.Namespace, supervisorOpts, a.loader, store, a.barrier)
			},
		}},
		Map: map[string]gen.Atom{"supervisor": a.SupervisorName()},
	}, nil
}

// registerMetrics creates every controller collector on radar's registry through the node, which owns
// them for the node's lifetime.
func (a *Application[T]) registerMetrics() {
	node := a.Node()
	if node == nil {
		return
	}
	if err := telemetry.Register(node, controllerMetrics); err != nil {
		a.Log().Debug("radar telemetry unavailable: namespace=%q error=%v", a.opts.Namespace, err)
	}
}

// Terminate only seals and reports. Waiting and closing belong to the service.
func (a *Application[T]) Terminate(reason error) {
	a.Seal()
	a.labels.Count(a.Node(), metricApplicationTerminations, telemetry.TerminationReason(reason))
	a.labels.Set(a.Node(), metricApplicationState, 0)
	a.Log().Info("controller application terminated: name=%s namespace=%q reason=%v", a.Name(), a.opts.Namespace, reason)
	select {
	case a.stopped <- reason:
	default:
	}
}

// Close closes the application-owned resources after Seal and quiescence are proven.
func (a *Application[T]) Close(ctx context.Context) error {
	if !a.barrier.Quiesced() {
		err := fmt.Errorf("controller application writer I/O has not quiesced")
		a.labels.Count(a.Node(), metricApplicationCloses, "rejected")
		a.Log().Error("controller application close rejected: name=%s namespace=%q error=%v", a.Name(), a.opts.Namespace, err)
		return err
	}
	database := a.database
	a.database = nil
	a.Log().Debug("controller application resources closing: name=%s namespace=%q database=%t", a.Name(), a.opts.Namespace, database != nil)
	done := make(chan error, 1)
	go func() {
		var err error
		if database != nil {
			err = database.Close()
		}
		if err != nil {
			a.Log().Error("controller application resource close failed: name=%s namespace=%q error=%v", a.Name(), a.opts.Namespace, err)
		} else {
			a.Log().Info("controller application resources closed: name=%s namespace=%q", a.Name(), a.opts.Namespace)
		}
		done <- err
	}()

	select {
	case err := <-done:
		a.labels.Count(a.Node(), metricApplicationCloses, telemetry.Result(err))
		return err
	case <-ctx.Done():
		a.labels.Count(a.Node(), metricApplicationCloses, "timeout")
		a.Log().Debug("controller application close wait interrupted: name=%s namespace=%q error=%v", a.Name(), a.opts.Namespace, ctx.Err())
		return ctx.Err()
	}
}
