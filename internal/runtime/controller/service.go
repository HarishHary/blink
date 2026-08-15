package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ergo.services/ergo/gen"
	errs "github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/runtime/plugin"
)

const serviceShutdownTimeout = 45 * time.Second

// Service runs one controller application for a plugin type.
type Service[T plugin.Syncable] struct {
	node            gen.Node
	name            string
	opts            ControllerApplicationOptions[T]
	shutdownTimeout time.Duration
}

// NewService creates a service that constructs a fresh application per Run.
func NewService[T plugin.Syncable](node gen.Node, name string, opts ControllerApplicationOptions[T]) *Service[T] {
	return &Service[T]{node: node, name: name, opts: opts, shutdownTimeout: serviceShutdownTimeout}
}

// Name identifies the service to the Runner.
func (s *Service[T]) Name() string { return s.name }

// Run loads, runs, and safely cleans up one application attempt.
func (s *Service[T]) Run(ctx context.Context) errs.Error {
	app := NewApplication(s.opts)
	name, err := s.node.ApplicationLoad(app)
	if err != nil {
		return errs.NewE(errors.Join(err, s.cleanupAttempt(ctx, app, "")))
	}
	if name != app.Name() {
		return errs.NewE(errors.Join(fmt.Errorf("loaded application name %q, want %q", name, app.Name()), s.cleanupAttempt(ctx, app, name)))
	}
	if err := s.node.ApplicationStart(name, gen.ApplicationOptions{}); err != nil {
		app.Seal()
		return errs.NewE(errors.Join(fmt.Errorf("start %s: %w", name, err), s.forceStop(name), s.cleanupAttempt(ctx, app, name)))
	}

	select {
	case <-ctx.Done():
		if err := errors.Join(s.gracefulStop(app, name), s.cleanupAttempt(ctx, app, name)); err != nil {
			return errs.NewE(err)
		}
		return nil
	case reason := <-app.Stopped():
		stoppedErr := fmt.Errorf("%s controller application stopped", name)
		if reason != nil {
			stoppedErr = fmt.Errorf("%s controller application stopped: %w", name, reason)
		}
		return errs.NewE(errors.Join(stoppedErr, s.cleanupAttempt(ctx, app, name)))
	}
}

// cleanupAttempt closes an application and unloads it while the root context is live.
func (s *Service[T]) cleanupAttempt(ctx context.Context, app *ControllerApplication[T], name gen.Atom) error {
	cleanupCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			timer := time.NewTimer(s.shutdownTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				cancel()
			case <-cleanupCtx.Done():
			}
		case <-cleanupCtx.Done():
		}
	}()
	app.Seal()
	if err := app.WaitQuiesced(cleanupCtx); err != nil {
		return err
	}
	if err := cleanupCtx.Err(); err != nil {
		return err
	}
	closeErr := app.Close(cleanupCtx)
	if ctx.Err() != nil {
		return closeErr
	}
	if err := cleanupCtx.Err(); err != nil {
		return err
	}
	if name != "" {
		for {
			if err := cleanupCtx.Err(); err != nil {
				return errors.Join(closeErr, err)
			}
			err := s.node.ApplicationUnload(name)
			if err == nil || errors.Is(err, gen.ErrApplicationUnknown) || errors.Is(err, gen.ErrNodeTerminated) {
				break
			}
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-timer.C:
			case <-cleanupCtx.Done():
				timer.Stop()
				return errors.Join(closeErr, err, cleanupCtx.Err())
			}
		}
	}
	return closeErr
}

// gracefulStop drains the application before forcing it to stop.
func (s *Service[T]) gracefulStop(app *ControllerApplication[T], name gen.Atom) error {
	app.Seal()
	pid, err := s.node.ProcessPID(app.SupervisorName())
	if err != nil {
		return errors.Join(fmt.Errorf("lookup supervisor %s: %w", app.SupervisorName(), err), s.forceStop(name))
	}
	if err := s.node.Send(pid, plugin.MessageStop{}); err != nil {
		return errors.Join(fmt.Errorf("stop supervisor %s: %w", app.SupervisorName(), err), s.forceStop(name))
	}

	timer := time.NewTimer(s.shutdownTimeout)
	defer timer.Stop()
	select {
	case <-app.Stopped():
		return nil
	case <-timer.C:
		return s.forceStop(name)
	}
}

// forceStop stops an application when graceful shutdown cannot finish.
func (s *Service[T]) forceStop(name gen.Atom) error {
	if err := s.node.ApplicationStopForce(name); err != nil {
		if errors.Is(err, gen.ErrApplicationUnknown) || errors.Is(err, gen.ErrNodeTerminated) || errors.Is(err, gen.ErrApplicationStopping) {
			return nil
		}
		return fmt.Errorf("force stop %s: %w", name, err)
	}
	return nil
}
