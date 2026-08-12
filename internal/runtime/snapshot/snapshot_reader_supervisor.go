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

// SnapshotReaderEvents identifies the two buffered events produced by a snapshot-reader
// supervisor. Snapshot carries *snapshot.Snapshot; Status carries
// SnapshotReaderStatus.
type SnapshotReaderEvents struct {
	Snapshot      gen.Event
	Status        gen.Event
	snapshotToken gen.Ref
	statusToken   gen.Ref
}

// EventsFor returns the stable event identifiers for a reader subtree.
func EventsFor(node gen.Node, name gen.Atom) SnapshotReaderEvents {
	return SnapshotReaderEvents{
		Snapshot: gen.Event{Name: gen.Atom(string(name) + "-snapshot"), Node: node.Name()},
		Status:   gen.Event{Name: gen.Atom(string(name) + "-status"), Node: node.Name()},
	}
}

// SnapshotReaderSupervisorOptions configures one supervised snapshot-reader subtree.
type SnapshotReaderSupervisorOptions struct {
	Name          gen.Atom
	Logger        *logger.Logger
	ReaderFactory func() brokers.Reader
	RestartMin    time.Duration
	RestartMax    time.Duration
}

type snapshotReaderActorState struct {
	pid    gen.PID
	status SnapshotReaderActorStatus
}

type snapshotReaderSupervisor struct {
	act.Supervisor

	opts SnapshotReaderSupervisorOptions

	actor snapshotReaderActorState

	events SnapshotReaderEvents
}

func NewSupervisor(opts SnapshotReaderSupervisorOptions) gen.ProcessBehavior {
	return &snapshotReaderSupervisor{opts: defaultOptions(opts)}
}

// --- messages ---

// The supervisor accepts status only from its current child PID.
type MessageSnapshotReaderStatusChanged struct{ status SnapshotReaderActorStatus }

// --- messages ---

func (s *snapshotReaderSupervisor) Init(...any) (act.SupervisorSpec, error) {
	if s.opts.Name == "" || s.opts.Logger == nil || s.opts.ReaderFactory == nil {
		return act.SupervisorSpec{}, fmt.Errorf("actor snapshot: name, logger, and reader factory are required")
	}

	s.events = EventsFor(s.Node(), s.opts.Name)

	snapshotToken, err := s.RegisterEvent(s.events.Snapshot.Name, gen.EventOptions{Buffer: 1})
	if err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot event: %w", err)
	}
	s.events.snapshotToken = snapshotToken

	statusToken, err := s.RegisterEvent(s.events.Status.Name, gen.EventOptions{Buffer: 1})
	if err != nil {
		return act.SupervisorSpec{}, fmt.Errorf("register snapshot status event: %w", err)
	}
	s.events.statusToken = statusToken
	s.actor.status = newSnapshotReaderStatus()
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

	s.actor.pid = pid
	s.actor.status = newSnapshotReaderStatus()
	s.publishStatus()

	return s.Send(pid, MessageSnapshotReaderActivate{
		snapshotEventName:  s.events.Snapshot.Name,
		snapshotEventToken: s.events.snapshotToken,
	})
}

func (s *snapshotReaderSupervisor) HandleChildTerminate(name gen.Atom, pid gen.PID, reason error) error {
	if name != s.readerActorName() || s.actor.pid != pid {
		return nil
	}

	s.actor.pid = gen.PID{}
	s.actor.status.Lifecycle = SnapshotReaderRestarting
	s.actor.status.Availability = runtime.AvailabilityUnavailable
	s.actor.status.LastError = reason
	s.actor.status.Reader.Lifecycle = SnapshotReaderMetaStopped
	s.actor.status.Reader.Availability = runtime.AvailabilityUnavailable
	s.actor.status.Reader.CaughtUp = false
	s.actor.status.Reader.RestartPending = false
	s.publishStatus()
	return nil
}

func (s *snapshotReaderSupervisor) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case MessageSnapshotReaderStatusChanged:
		if from != s.actor.pid {
			return nil
		}
		s.actor.status = m.status
		s.publishStatus()
	}
	return nil
}

func (s *snapshotReaderSupervisor) HandleCall(_ gen.PID, _ gen.Ref, request any) (any, error) {
	return nil, fmt.Errorf("snapshot reader supervisor: unsupported call %T", request)
}

func (s *snapshotReaderSupervisor) Terminate(error) {
	s.actor.status.Lifecycle = SnapshotReaderStopped
	s.actor.status.Availability = runtime.AvailabilityUnavailable
	s.actor.status.Reader.Lifecycle = SnapshotReaderMetaStopped
	s.actor.status.Reader.Availability = runtime.AvailabilityUnavailable
	s.publishStatus()
}

func (s *snapshotReaderSupervisor) readerActorName() gen.Atom {
	return gen.Atom(string(s.opts.Name) + "-reader")
}

func (s *snapshotReaderSupervisor) publishStatus() {
	if s.events.statusToken == (gen.Ref{}) {
		return
	}
	_ = s.SendEvent(s.events.Status.Name, s.events.statusToken, s.actor.status)
}

func newSnapshotReaderStatus() SnapshotReaderActorStatus {
	return SnapshotReaderActorStatus{
		Lifecycle:    SnapshotReaderStarting,
		Availability: runtime.AvailabilityUnavailable,
		Reader: SnapshotReaderMetaStatus{
			Lifecycle:    SnapshotReaderMetaStarting,
			Availability: runtime.AvailabilityUnavailable,
		},
	}
}

func defaultOptions(opts SnapshotReaderSupervisorOptions) SnapshotReaderSupervisorOptions {
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
