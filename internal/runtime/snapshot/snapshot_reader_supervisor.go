// Package actorsnapshot provides an Ergo-owned reader for Blink's compacted
// control-plane snapshot topics.
package snapshot

import (
	"fmt"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/runtime"
)

const (
	snapshotReaderActorRestartIntensity uint16 = 5
	snapshotReaderActorRestartPeriod    uint16 = 10
)

// Options configures one supervised snapshot-reader subtree.
type Options struct {
	Name          gen.Atom
	Logger        *logger.Logger
	ReaderFactory func() brokers.Reader
	RestartMin    time.Duration
	RestartMax    time.Duration
}

// Events identifies the two buffered events produced by a snapshot-reader
// supervisor. Snapshot carries *snapshot.Snapshot; Status carries
// SnapshotReaderStatus.
type Events struct {
	Snapshot gen.Event
	Status   gen.Event
}

// EventsFor returns the stable event identifiers for a reader subtree.
func EventsFor(node gen.Node, name gen.Atom) Events {
	return Events{
		Snapshot: gen.Event{Name: gen.Atom(string(name) + "-snapshot"), Node: node.Name()},
		Status:   gen.Event{Name: gen.Atom(string(name) + "-status"), Node: node.Name()},
	}
}

// SnapshotReaderLifecycle describes the stable snapshot-reader actor subtree.
type SnapshotReaderLifecycle string

const (
	SnapshotReaderStarting   SnapshotReaderLifecycle = "starting"
	SnapshotReaderRunning    SnapshotReaderLifecycle = "running"
	SnapshotReaderRestarting SnapshotReaderLifecycle = "restarting"
	SnapshotReaderStopped    SnapshotReaderLifecycle = "stopped"
)

// SnapshotReaderStatus is the public status value published by the supervisor.
// Reader contains the independently managed broker-reader meta-process state.
type SnapshotReaderStatus struct {
	Lifecycle         SnapshotReaderLifecycle
	Availability      runtime.Availability
	ActorGeneration   uint64
	ActorLastError    string
	LocalRevision     int64
	AppliedGeneration int64
	Reader            SnapshotReaderMetaStatus
}

// snapshotReaderActorStatusChanged is emitted by the current reader actor. The
// supervisor validates the exact PID and actor generation before publishing.
type snapshotReaderActorStatusChanged struct {
	pid        gen.PID
	generation uint64
	status     SnapshotReaderStatus
}

// snapshotReaderActorActivate delegates the supervisor-owned snapshot event
// token to the current actor generation.
type snapshotReaderActorActivate struct {
	generation         uint64
	revision           int64
	snapshotEventName  gen.Atom
	snapshotEventToken gen.Ref
}

// NewSupervisor creates the root behavior for:
//
//	snapshotReaderSupervisor
//	└── snapshotReaderActor
//	    └── snapshotReaderMeta
func NewSupervisor(opts Options) gen.ProcessBehavior {
	return &snapshotReaderSupervisor{opts: defaultOptions(opts)}
}

type snapshotReaderSupervisor struct {
	act.Supervisor

	opts Options

	actorPID        gen.PID
	actorGeneration uint64

	events        Events
	snapshotToken gen.Ref
	statusToken   gen.Ref
	status        SnapshotReaderStatus
}

func (s *snapshotReaderSupervisor) Init(...any) (act.SupervisorSpec, error) {
	if s.opts.Name == "" || s.opts.Logger == nil || s.opts.ReaderFactory == nil {
		return act.SupervisorSpec{}, fmt.Errorf("actor snapshot: name, logger, and reader factory are required")
	}

	s.events = EventsFor(s.Node(), s.opts.Name)

	snapshotToken, err := s.RegisterEvent(s.events.Snapshot.Name, gen.EventOptions{Buffer: 1})
	if err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot event: %w", err)
	}
	s.snapshotToken = snapshotToken

	statusToken, err := s.RegisterEvent(s.events.Status.Name, gen.EventOptions{Buffer: 1})
	if err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot status event: %w", err)
	}
	s.statusToken = statusToken
	s.status = newSnapshotReaderStatus(0)
	s.publishStatus()

	return act.SupervisorSpec{
		Type:                act.SupervisorTypeOneForOne,
		EnableHandleChild:   true,
		DisableAutoShutdown: true,
		Restart: act.SupervisorRestart{
			Strategy:  act.SupervisorStrategyPermanent,
			Intensity: snapshotReaderActorRestartIntensity,
			Period:    snapshotReaderActorRestartPeriod,
		},
		Children: []act.SupervisorChildSpec{{
			Name: s.readerActorName(),
			Factory: func() gen.ProcessBehavior {
				return &snapshotReaderActor{opts: s.opts}
			},
			Options: gen.ProcessOptions{},
		}},
	}, nil
}

func (s *snapshotReaderSupervisor) HandleChildStart(name gen.Atom, pid gen.PID) error {
	if name != s.readerActorName() {
		return nil
	}

	s.actorPID = pid
	s.actorGeneration++
	s.status = newSnapshotReaderStatus(s.actorGeneration)
	s.publishStatus()

	return s.Send(pid, snapshotReaderActorActivate{
		generation:         s.actorGeneration,
		revision:           int64(s.actorGeneration) << 32,
		snapshotEventName:  s.events.Snapshot.Name,
		snapshotEventToken: s.snapshotToken,
	})
}

func (s *snapshotReaderSupervisor) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	if name != s.readerActorName() || s.actorPID != pid {
		return nil
	}

	s.actorPID = gen.PID{}
	s.status.Lifecycle = SnapshotReaderRestarting
	s.status.Availability = runtime.AvailabilityUnavailable
	s.status.ActorGeneration = s.actorGeneration
	s.status.ActorLastError = errorText(reason)
	s.status.Reader.Lifecycle = SnapshotReaderMetaStopped
	s.status.Reader.Availability = runtime.AvailabilityUnavailable
	s.status.Reader.CaughtUp = false
	s.status.Reader.RestartPending = false
	s.publishStatus()
	return nil
}

func (s *snapshotReaderSupervisor) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case snapshotReaderActorStatusChanged:
		if from != s.actorPID ||
			m.pid != s.actorPID ||
			m.generation != s.actorGeneration {
			return nil
		}
		s.status = m.status
		s.publishStatus()
	}
	return nil
}

func (s *snapshotReaderSupervisor) HandleCall(gen.PID, gen.Ref, any) (any, error) {
	return nil, nil
}

func (s *snapshotReaderSupervisor) Terminate(error) {
	s.status.Lifecycle = SnapshotReaderStopped
	s.status.Availability = runtime.AvailabilityUnavailable
	s.status.Reader.Lifecycle = SnapshotReaderMetaStopped
	s.status.Reader.Availability = runtime.AvailabilityUnavailable
	s.publishStatus()
}

func (s *snapshotReaderSupervisor) readerActorName() gen.Atom {
	return gen.Atom(string(s.opts.Name) + "-reader")
}

func (s *snapshotReaderSupervisor) publishStatus() {
	if s.statusToken == (gen.Ref{}) {
		return
	}
	_ = s.SendEvent(s.events.Status.Name, s.statusToken, s.status)
}

func newSnapshotReaderStatus(actorGeneration uint64) SnapshotReaderStatus {
	return SnapshotReaderStatus{
		Lifecycle:       SnapshotReaderStarting,
		Availability:    runtime.AvailabilityUnavailable,
		ActorGeneration: actorGeneration,
		Reader: SnapshotReaderMetaStatus{
			Lifecycle:    SnapshotReaderMetaStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func defaultOptions(opts Options) Options {
	if opts.Name == "" {
		opts.Name = "snapshot-reader"
	}
	if opts.RestartMin <= 0 {
		opts.RestartMin = 100 * time.Millisecond
	}
	if opts.RestartMax <= 0 {
		opts.RestartMax = 5 * time.Second
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = opts.RestartMin
	}
	return opts
}
