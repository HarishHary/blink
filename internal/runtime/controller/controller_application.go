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
)

// Application owns the resources for one plugin-type controller application.
type Application[T plugin.Artifact] struct {
	app.Application
	opts     ApplicationOptions[T]
	database *sql.DB
	barrier  *writerIOBarrier
	stopped  chan error
	scope    metricScope
}

// NewApplication creates an unloaded application for one plugin type.
func NewApplication[T plugin.Artifact](opts ApplicationOptions[T]) *Application[T] {
	normalized := applicationOptionsWithDefaults(opts)
	return &Application[T]{
		opts:    normalized,
		barrier: newWriterIOBarrier(),
		stopped: make(chan error, 1),
		scope:   newMetricScope(normalized.Namespace),
	}
}

// Name returns the application name after defaults are applied.
func (a *Application[T]) Name() gen.Atom { return a.opts.Name }

// SupervisorName returns the root supervisor name after defaults are applied.
func (a *Application[T]) SupervisorName() gen.Atom { return a.opts.SupervisorOptions.Name }

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
	a.Log().Debug("controller application loading: name=%s namespace=%q", a.opts.Name, a.opts.Namespace)
	a.registerMetrics()
	if a.opts.Name == "" || a.opts.SupervisorOptions.Name == "" || a.opts.Namespace == "" {
		err := fmt.Errorf("controller application: name, supervisor name, and namespace are required")
		a.scope.count(a.Node(), metricApplicationLoads, "invalid")
		a.Log().Error("controller application configuration invalid: name=%s namespace=%q error=%v", a.opts.Name, a.opts.Namespace, err)
		return gen.ApplicationSpec{}, err
	}

	database, err := backends.OpenSQLite(a.opts.DatabaseDSN)
	if err != nil {
		a.scope.count(a.Node(), metricApplicationLoads, "database")
		a.Log().Error("controller application database open failed: name=%s namespace=%q error=%v", a.opts.Name, a.opts.Namespace, err)
		return gen.ApplicationSpec{}, fmt.Errorf("open %s controller database: %w", a.opts.Namespace, err)
	}
	store, err := backends.NewSQLite(database, a.opts.Namespace)
	if err != nil {
		_ = database.Close()
		a.scope.count(a.Node(), metricApplicationLoads, "database")
		a.Log().Error("controller application database initialization failed: name=%s namespace=%q error=%v", a.opts.Name, a.opts.Namespace, err)
		return gen.ApplicationSpec{}, fmt.Errorf("initialize %s controller database: %w", a.opts.Namespace, err)
	}

	a.database = database
	supervisorOpts := a.opts.SupervisorOptions
	a.scope.count(a.Node(), metricApplicationLoads, "ok")
	a.scope.set(a.Node(), metricApplicationState, 1)
	a.Log().Info("controller application loaded: name=%s namespace=%q supervisor=%s", a.opts.Name, a.opts.Namespace, a.opts.SupervisorOptions.Name)

	return gen.ApplicationSpec{
		Name:        a.opts.Name,
		Description: fmt.Sprintf("Blink %s controller", a.opts.Namespace),
		Mode:        gen.ApplicationModePermanent,
		Network:     gen.ApplicationNetwork{RegisterTypes: snapshot.NetworkTypes()},
		Group: []gen.ApplicationMemberSpec{{
			Name: a.opts.SupervisorOptions.Name,
			Factory: func() gen.ProcessBehavior {
				return newSupervisor(supervisorOpts, store, a.barrier)
			},
		}},
		Map: map[string]gen.Atom{"supervisor": a.opts.SupervisorOptions.Name},
	}, nil
}

// registerMetrics creates every controller collector on radar's registry through the node, which owns them for the node's lifetime.
func (a *Application[T]) registerMetrics() {
	node := a.Node()
	if node == nil {
		return
	}
	if err := registerMetrics(node); err != nil {
		a.Log().Debug("radar telemetry unavailable: namespace=%q error=%v", a.opts.Namespace, err)
	}
}

// Terminate only seals and reports. Waiting and closing belong to the service.
func (a *Application[T]) Terminate(reason error) {
	a.Seal()
	a.scope.count(a.Node(), metricApplicationTerminations, terminationReason(reason))
	a.scope.set(a.Node(), metricApplicationState, 0)
	a.Log().Info("controller application terminated: name=%s namespace=%q reason=%v", a.opts.Name, a.opts.Namespace, reason)
	select {
	case a.stopped <- reason:
	default:
	}
}

// Close closes the application-owned resources after Seal and quiescence are proven.
func (a *Application[T]) Close(ctx context.Context) error {
	if !a.barrier.Quiesced() {
		err := fmt.Errorf("controller application writer I/O has not quiesced")
		a.scope.count(a.Node(), metricApplicationCloses, "rejected")
		a.Log().Error("controller application close rejected: name=%s namespace=%q error=%v", a.opts.Name, a.opts.Namespace, err)
		return err
	}
	database := a.database
	a.database = nil
	a.Log().Debug("controller application resources closing: name=%s namespace=%q database=%t", a.opts.Name, a.opts.Namespace, database != nil)
	done := make(chan error, 1)
	go func() {
		var err error
		if database != nil {
			err = database.Close()
		}
		if err != nil {
			a.Log().Error("controller application resource close failed: name=%s namespace=%q error=%v", a.opts.Name, a.opts.Namespace, err)
		} else {
			a.Log().Info("controller application resources closed: name=%s namespace=%q", a.opts.Name, a.opts.Namespace)
		}
		done <- err
	}()

	select {
	case err := <-done:
		a.scope.count(a.Node(), metricApplicationCloses, metricResult(err))
		return err
	case <-ctx.Done():
		a.scope.count(a.Node(), metricApplicationCloses, "timeout")
		a.Log().Debug("controller application close wait interrupted: name=%s namespace=%q error=%v", a.opts.Name, a.opts.Namespace, ctx.Err())
		return ctx.Err()
	}
}
