package snapshot

// Loader supplies the type-specific behavior owned by a projection actor.
// ParseSpec and Clone must not retain references to their inputs.
type Loader[T any] interface {
	ParseSpec(name string, spec []byte) (T, error)
	Clone(T) T
	MaxProcs(T) int
	// CallsPerProcess reports how many invocations one of those processes may run at once, which
	// together with MaxProcs is the deployment's own ceiling on concurrent calls.
	CallsPerProcess(T) int
	// RolloutPct reports the share of rollout buckets this artifact claims as a canary candidate,
	// which is the same number the router routes each call by.
	RolloutPct(T) float64
}
