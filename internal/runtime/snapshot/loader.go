package snapshot

// Loader supplies the type-specific behavior owned by a projection actor; ParseSpec and Clone must
// not retain references to their inputs.
type Loader[T any] interface {
	// ParseSpec turns one artifact's marshaled spec into owned typed data.
	ParseSpec(name string, spec []byte) (T, error)
	// Clone returns an independent copy of that data.
	Clone(T) T
	// MaxProcs is how many processes this artifact's deployment may run.
	MaxProcs(T) int
	// CallsPerProcess is how many invocations one of those processes may run at once.
	CallsPerProcess(T) int
	// RolloutPct is the share of rollout buckets this artifact claims as a canary candidate.
	RolloutPct(T) float64
}
