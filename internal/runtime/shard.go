package runtime

import "sync"

// MaxCallPayloadBytes is the payload one invocation may carry, a quarter under the 4 MiB gRPC
// default go-plugin leaves in place: a caller estimates the encoding, and framing sits outside it.
const MaxCallPayloadBytes = 3 << 20

// MaxChunks is how many pieces a batch should be cut into: one per unit of concurrency, or more
// where the payload budget requires it. Only the budget may exceed workers, so a pool has to run
// them. itemBytes of zero asks for the concurrency reason alone.
func MaxChunks(items, workers, itemBytes int) int {
	if items <= 1 {
		return 1
	}
	forPayload := 1
	if itemBytes > 0 {
		perCall := max(1, MaxCallPayloadBytes/itemBytes)
		forPayload = (items + perCall - 1) / perCall
	}
	return min(items, max(1, workers, forPayload))
}

func ShardSlice[T any](items []T, maxChunks int) [][]T {
	if maxChunks < 1 {
		maxChunks = 1
	}
	base := len(items) / maxChunks
	extra := len(items) % maxChunks
	chunks := make([][]T, 0, maxChunks)
	start := 0
	for i := 0; i < maxChunks; i++ {
		size := base
		if i < extra {
			size++
		}
		if size == 0 {
			continue
		}
		chunks = append(chunks, items[start:start+size])
		start += size
	}
	return chunks
}

// ShardPooled cuts items into maxChunks balanced pieces and runs at most workers of them at a time,
// returning one result per piece in input order. Fewer workers than pieces costs only wall-clock.
func ShardPooled[T, R any](items []T, maxChunks, workers int, fn func([]T) R) []R {
	if maxChunks > len(items) {
		maxChunks = len(items)
	}
	if maxChunks <= 1 {
		return []R{fn(items)}
	}
	pieces := ShardSlice(items, maxChunks)
	results := make([]R, len(pieces))
	if workers <= 1 {
		// A worker running these serially would only add a handoff to the caller's own goroutine.
		for i, piece := range pieces {
			results[i] = fn(piece)
		}
		return results
	}
	// Every index is buffered before a worker exists, so the close only says when to stop.
	queue := make(chan int, len(pieces))
	for i := range pieces {
		queue <- i
	}
	close(queue)
	var wg sync.WaitGroup
	for range min(workers, len(pieces)) {
		wg.Go(func() {
			for i := range queue {
				results[i] = fn(pieces[i])
			}
		})
	}
	wg.Wait()
	return results
}
