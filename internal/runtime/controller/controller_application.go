package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"ergo.services/ergo/app"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime/plugin"
)

// ControllerApplication owns the resources for one plugin-type controller application.
type ControllerApplication[T plugin.Syncable] struct {
	app.Application
	opts     ControllerApplicationOptions[T]
	database *sql.DB
	writer   brokers.Writer
	barrier  *publisherIOBarrier
	stopped  chan error
}

// NewApplication creates an unloaded application for one plugin type.
func NewApplication[T plugin.Syncable](opts ControllerApplicationOptions[T]) *ControllerApplication[T] {
	return &ControllerApplication[T]{
		opts:    controllerApplicationOptionsWithDefaults(opts),
		barrier: newPublisherIOBarrier(),
		stopped: make(chan error, 1),
	}
}

// Name returns the application name after defaults are applied.
func (a *ControllerApplication[T]) Name() gen.Atom { return a.opts.Name }

// SupervisorName returns the root supervisor name after defaults are applied.
func (a *ControllerApplication[T]) SupervisorName() gen.Atom { return a.opts.SupervisorOptions.Name }

// Stopped reports the application callback without blocking the Ergo runtime.
func (a *ControllerApplication[T]) Stopped() <-chan error { return a.stopped }

// Seal prevents new publisher I/O from starting for this application attempt.
func (a *ControllerApplication[T]) Seal() { a.barrier.Seal() }

// WaitQuiesced waits for publisher I/O accepted before Seal to finish.
func (a *ControllerApplication[T]) WaitQuiesced(ctx context.Context) error {
	return a.barrier.WaitQuiesced(ctx)
}

// Load opens the application-owned resources and describes its one supervisor.
func (a *ControllerApplication[T]) Load(_ ...any) (gen.ApplicationSpec, error) {
	if a.opts.Name == "" || a.opts.SupervisorOptions.Name == "" || a.opts.Namespace == "" || a.opts.Topic == "" || a.opts.Broker == nil {
		return gen.ApplicationSpec{}, fmt.Errorf("controller application: name, supervisor name, namespace, topic, and broker are required")
	}

	database, err := backends.OpenSQLite(a.opts.DatabaseDSN)
	if err != nil {
		return gen.ApplicationSpec{}, fmt.Errorf("open %s controller database: %w", a.opts.Namespace, err)
	}
	store, err := backends.NewSQLite(database, a.opts.Namespace)
	if err != nil {
		_ = database.Close()
		return gen.ApplicationSpec{}, fmt.Errorf("initialize %s controller database: %w", a.opts.Namespace, err)
	}

	writer := a.opts.Broker.NewWriter(a.opts.Topic)
	a.database = database
	a.writer = writer
	supervisorOpts := a.opts.SupervisorOptions

	return gen.ApplicationSpec{
		Name:        a.opts.Name,
		Description: fmt.Sprintf("Blink %s controller", a.opts.Namespace),
		Mode:        gen.ApplicationModeTransient,
		Group: []gen.ApplicationMemberSpec{{
			Name: a.opts.SupervisorOptions.Name,
			Factory: func() gen.ProcessBehavior {
				return newSupervisor(supervisorOpts, store, writer, a.barrier)
			},
		}},
		Map: map[string]gen.Atom{"supervisor": a.opts.SupervisorOptions.Name},
	}, nil
}

// Terminate only seals and reports. Waiting and closing belong to the service.
func (a *ControllerApplication[T]) Terminate(reason error) {
	a.Seal()
	select {
	case a.stopped <- reason:
	default:
	}
}

// Close closes the application-owned resources after Seal and quiescence are proven.
func (a *ControllerApplication[T]) Close(ctx context.Context) error {
	if !a.barrier.Quiesced() {
		return fmt.Errorf("controller application publisher I/O has not quiesced")
	}
	writer, database := a.writer, a.database
	a.writer, a.database = nil, nil
	done := make(chan error, 1)
	go func() {
		type closeResult struct {
			resource string
			err      error
		}
		results := make(chan closeResult, 2)
		pending := 0
		if writer != nil {
			pending++
			go func() { results <- closeResult{"writer", writer.Close()} }()
		}
		if database != nil {
			pending++
			go func() { results <- closeResult{"database", database.Close()} }()
		}
		var errs []error
		for range pending {
			result := <-results
			if result.err != nil {
				errs = append(errs, fmt.Errorf("close %s: %w", result.resource, result.err))
			}
		}
		done <- errors.Join(errs...)
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
