// Package actorstatus contains status primitives shared by independent Ergo
// actor subtrees without coupling those subtrees to each other's packages.
package runtime

import "time"

// ErrorText returns an empty string for nil errors and the error text otherwise.
func ErrorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// FirstError returns the first active failure in the caller's priority order.
func FirstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// NextStatusEpoch returns a Unix-nanosecond status version. Repeated or backward
// clock readings advance one nanosecond past the previous version instead.
func NextStatusEpoch(previous int64) int64 {
	return max(time.Now().UnixNano(), previous+1)
}

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
