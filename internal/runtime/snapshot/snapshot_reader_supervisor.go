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

// The supervisor accepts status only from its current child PID.
type MessageSnapshotReaderStatusChanged struct{ status SnapshotReaderStatus }

// MessageSnapshotReaderActivate delegates the supervisor-owned snapshot event
// token to the current reader actor.
type MessageSnapshotReaderActivate struct {
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

	actorPID gen.PID

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
	s.status = newSnapshotReaderStatus()
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
	s.status = newSnapshotReaderStatus()
	s.publishStatus()

	return s.Send(pid, MessageSnapshotReaderActivate{
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
	s.status.LastError = reason
	s.status.Reader.Lifecycle = SnapshotReaderMetaStopped
	s.status.Reader.Availability = runtime.AvailabilityUnavailable
	s.status.Reader.CaughtUp = false
	s.status.Reader.RestartPending = false
	s.publishStatus()
	return nil
}

func (s *snapshotReaderSupervisor) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessageSnapshotReaderStatusChanged:
		if from != s.actorPID {
			return nil
		}
		s.status = m.status
		s.publishStatus()
	}
	return nil
}

func (s *snapshotReaderSupervisor) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return nil, fmt.Errorf("snapshot reader supervisor: unsupported call %T", request)
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

func newSnapshotReaderStatus() SnapshotReaderStatus {
	return SnapshotReaderStatus{
		Lifecycle:    SnapshotReaderStarting,
		Availability: runtime.AvailabilityUnavailable,
		Reader: SnapshotReaderMetaStatus{
			Lifecycle:    SnapshotReaderMetaStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
	}
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
