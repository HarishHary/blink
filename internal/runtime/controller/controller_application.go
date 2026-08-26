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
	opts     Options[T]
	database *sql.DB
	barrier  *writerIOBarrier
	stopped  chan error
}

// NewApplication creates an unloaded application for one plugin type.
func NewApplication[T plugin.Artifact](opts Options[T]) *Application[T] {
	return &Application[T]{
		opts:    optionsWithDefaults(opts),
		barrier: newWriterIOBarrier(),
		stopped: make(chan error, 1),
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
	if a.opts.Name == "" || a.opts.SupervisorOptions.Name == "" || a.opts.Namespace == "" {
		err := fmt.Errorf("controller application: name, supervisor name, and namespace are required")
		a.Log().Error("controller application configuration invalid: name=%s namespace=%q error=%v", a.opts.Name, a.opts.Namespace, err)
		return gen.ApplicationSpec{}, err
	}

	database, err := backends.OpenSQLite(a.opts.DatabaseDSN)
	if err != nil {
		a.Log().Error("controller application database open failed: name=%s namespace=%q error=%v", a.opts.Name, a.opts.Namespace, err)
		return gen.ApplicationSpec{}, fmt.Errorf("open %s controller database: %w", a.opts.Namespace, err)
	}
	store, err := backends.NewSQLite(database, a.opts.Namespace)
	if err != nil {
		_ = database.Close()
		a.Log().Error("controller application database initialization failed: name=%s namespace=%q error=%v", a.opts.Name, a.opts.Namespace, err)
		return gen.ApplicationSpec{}, fmt.Errorf("initialize %s controller database: %w", a.opts.Namespace, err)
	}

	a.database = database
	supervisorOpts := a.opts.SupervisorOptions
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

// Terminate only seals and reports. Waiting and closing belong to the service.
func (a *Application[T]) Terminate(reason error) {
	a.Seal()
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
		return err
	case <-ctx.Done():
		a.Log().Debug("controller application close wait interrupted: name=%s namespace=%q error=%v", a.opts.Name, a.opts.Namespace, ctx.Err())
		return ctx.Err()
	}
}
