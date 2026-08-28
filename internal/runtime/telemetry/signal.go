package telemetry

import (
	"time"

	"ergo.services/application/radar"
	"ergo.services/ergo/gen"
)

// RadarTickInterval paces a layer's radar session: retrying a registration radar was not up to
// accept, and beating a registered signal. SignalTimeout is radar's deadline for a missed beat, three
// ticks so one missed beat does not flip readiness.
const (
	RadarTickInterval = 30 * time.Second
	SignalTimeout     = 3 * RadarTickInterval
)

// Signal is one radar readiness signal, held down rather than unregistered on a drain.
type Signal struct {
	name       gen.Atom
	registered bool
	up         bool
}

// NewSignal names an unregistered signal.
func NewSignal(name gen.Atom) Signal {
	return Signal{name: name}
}

// Register creates the signal on radar at most once; radar marks a fresh registration up.
func (s *Signal) Register(process gen.Process) error {
	if s.registered {
		return nil
	}
	// Readiness, never liveness: a process that cannot reach a dependency must not be killed.
	if err := radar.RegisterService(process, s.name, radar.ProbeReadiness, SignalTimeout); err != nil {
		return err
	}
	s.registered, s.up = true, true
	return nil
}

// SetReady moves the signal only on a change; a resend would log a transition that did not happen.
func (s *Signal) SetReady(process gen.Process, ready bool) {
	if !s.registered || ready == s.up {
		return
	}
	s.up = ready
	if ready {
		_ = radar.ServiceUp(process, s.name)
		return
	}
	_ = radar.ServiceDown(process, s.name)
}

// Heartbeat refreshes the deadline, skipped while down: radar treats a beat as a recovery.
func (s *Signal) Heartbeat(process gen.Process) {
	if !s.registered || !s.up {
		return
	}
	_ = radar.Heartbeat(process, s.name)
}

// Registered reports whether radar has accepted this signal.
func (s Signal) Registered() bool {
	return s.registered
}

// State distinguishes a signal radar never accepted from one deliberately held down.
func (s Signal) State() string {
	switch {
	case !s.registered:
		return "unregistered"
	case s.up:
		return "up"
	default:
		return "down"
	}
}
