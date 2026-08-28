package runtime

import "ergo.services/ergo/gen"

// EventPublication is one registered event and the token SendEvent requires to publish through it:
// the pair a supervisor keeps for the events it publishes itself, and hands to the child that
// publishes in its place.
type EventPublication struct {
	Name  gen.Atom
	Token gen.Ref
}

// Registered reports whether the event was registered and its token handed over.
func (p EventPublication) Registered() bool { return p.Token != (gen.Ref{}) }
