// Package actorstatus contains status primitives shared by independent Ergo
// actor subtrees without coupling those subtrees to each other's packages.
package runtime

// Availability describes whether a component currently has usable serving
// capacity. Component-specific lifecycle enums remain separate.
type Availability string

const (
	AvailabilityUnavailable Availability = "unavailable"
	AvailabilityDegraded    Availability = "degraded"
	AvailabilityReady       Availability = "ready"
)

// Routable reports whether existing capacity may still receive work.
func (a Availability) Routable() bool {
	return a == AvailabilityReady || a == AvailabilityDegraded
}
