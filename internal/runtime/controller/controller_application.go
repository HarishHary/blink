package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/runtime/plugin"
)

// ControllerApplication owns the resources for one plugin-type controller application.
type ControllerApplication[T plugin.Syncable] struct {
	opts ControllerApplicationOptions[T]

	mu       sync.Mutex
	database *sql.DB
	writer   brokers.Writer
	started  bool
	drained  bool
	closed   bool
	stopped  chan ControllerSupervisorStopped
}

// NewApplication creates an unloaded application for one plugin type.
func NewApplication[T plugin.Syncable](opts ControllerApplicationOptions[T]) *ControllerApplication[T] {
	return &ControllerApplication[T]{opts: controllerApplicationOptionsWithDefaults(opts), stopped: make(chan ControllerSupervisorStopped, 1)}
}

// Name returns the application name after defaults are applied.
func (a *ControllerApplication[T]) Name() gen.Atom { return a.opts.Name }

// SupervisorName returns the root supervisor name after defaults are applied.
func (a *ControllerApplication[T]) SupervisorName() gen.Atom { return a.opts.SupervisorName }

// Stopped reports the supervisor's terminal state.
func (a *ControllerApplication[T]) Stopped() <-chan ControllerSupervisorStopped { return a.stopped }

// Load opens the application-owned resources and describes its one supervisor.
func (a *ControllerApplication[T]) Load(_ gen.Node, _ ...any) (gen.ApplicationSpec, error) {
	if a.opts.Name == "" || a.opts.SupervisorName == "" || a.opts.Namespace == "" || a.opts.Topic == "" || a.opts.Broker == nil {
		return gen.ApplicationSpec{}, fmt.Errorf("controller application: name, supervisor name, namespace, topic, and broker are required")
	}

	database, err := backends.OpenSQLite(a.opts.DatabaseDSN)
	if err != nil {
		return gen.ApplicationSpec{}, fmt.Errorf("open %s controller database: %w", a.opts.Namespace, err)
	}
	database.SetMaxOpenConns(1)
	store, err := backends.NewSQLite(database, a.opts.Namespace)
	if err != nil {
		_ = database.Close()
		return gen.ApplicationSpec{}, fmt.Errorf("initialize %s controller database: %w", a.opts.Namespace, err)
	}

	a.mu.Lock()
	if a.database != nil || a.closed {
		a.mu.Unlock()
		_ = database.Close()
		return gen.ApplicationSpec{}, fmt.Errorf("controller application %s already loaded", a.opts.Name)
	}
	a.database = database
	a.writer = a.opts.Broker.NewWriter(a.opts.Topic)
	controllerOpts := a.opts.Actor
	controllerOpts.Database = store
	controllerOpts.Writer = a.writer
	a.mu.Unlock()

	return gen.ApplicationSpec{
		Name:        a.opts.Name,
		Description: fmt.Sprintf("Blink %s controller", a.opts.Namespace),
		Mode:        gen.ApplicationModeTransient,
		Group: []gen.ApplicationMemberSpec{{
			Name: a.opts.SupervisorName,
			Factory: func() gen.ProcessBehavior {
				return NewSupervisor(ControllerSupervisorOptions[T]{
					ActorName:    a.opts.ActorName,
					ActorOptions: controllerOpts,
					OnStopped:    a.recordStopped,
				})
			},
		}},
		Map: map[string]gen.Atom{"supervisor": a.opts.SupervisorName},
	}, nil
}

// Start records that resource cleanup must wait for a proven controller drain.
func (a *ControllerApplication[T]) Start(gen.ApplicationMode) {
	a.mu.Lock()
	a.started = true
	a.mu.Unlock()
}

// Terminate cannot close started application resources because Ergo invokes it
// before the supervisor's Terminate callback records the drain proof.
func (a *ControllerApplication[T]) Terminate(error) {}

// Close closes application-owned resources before start or after a proven drain.
func (a *ControllerApplication[T]) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	if a.started && !a.drained {
		a.mu.Unlock()
		return errors.New("controller application has not drained")
	}
	writer, database := a.writer, a.database
	a.writer, a.database = nil, nil
	a.closed = true
	a.mu.Unlock()

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
		select {
		case result := <-results:
			if result.err != nil {
				errs = append(errs, fmt.Errorf("close %s: %w", result.resource, result.err))
			}
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		}
	}
	return errors.Join(errs...)
}

func (a *ControllerApplication[T]) recordStopped(stopped ControllerSupervisorStopped) {
	a.mu.Lock()
	a.drained = stopped.Drained
	a.mu.Unlock()
	select {
	case a.stopped <- stopped:
	default:
	}
}
