package snapshot

// Loader supplies the type-specific behavior owned by a projection actor.
// ParseSpec and Clone must not retain references to their inputs.
type Loader[T any] interface {
	ParseSpec(name string, spec []byte) (T, error)
	Clone(T) T
	MaxProcs(T) int
}
