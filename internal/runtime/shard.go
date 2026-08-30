package runtime

import "sync"

// MaxCallPayloadBytes is the payload one invocation may carry, a quarter under go-plugin's 4 MiB gRPC
// default.
const MaxCallPayloadBytes = 3 << 20

// ChunkBounds cuts sizes into pieces of at most maxBytes, as pieces+1 offsets; count balances them
// while the batch fits one call.
func ChunkBounds(sizes []int, maxBytes, workers int) []int {
	items := len(sizes)
	if items <= 1 {
		return []int{0, items}
	}
	total := 0
	for _, size := range sizes {
		total += size
	}
	workers = max(1, workers)
	if maxBytes <= 0 || total <= maxBytes {
		return countBounds(items, min(items, workers))
	}
	// Cut before the item that would cross the budget, the one rule no distribution of sizes can overflow.
	bounds := []int{0}
	run := 0
	for i, size := range sizes {
		if i > bounds[len(bounds)-1] && run+size > maxBytes {
			bounds = append(bounds, i)
			run = 0
		}
		run += size
	}
	bounds = append(bounds, items)
	pieces := len(bounds) - 1
	if pieces >= workers {
		return bounds
	}
	// Bytes left fewer calls than workers, so split each piece by count; that only lowers the bytes a
	// call carries.
	refined := make([]int, 0, workers+1)
	refined = append(refined, 0)
	per := (workers + pieces - 1) / pieces
	for i := 1; i <= pieces; i++ {
		start, end := bounds[i-1], bounds[i]
		for _, cut := range countBounds(end-start, min(end-start, per))[1:] {
			refined = append(refined, start+cut)
		}
	}
	return refined
}

// countBounds splits items into pieces balanced by count.
func countBounds(items, pieces int) []int {
	pieces = max(1, pieces)
	base := items / pieces
	extra := items % pieces
	bounds := make([]int, 1, pieces+1)
	start := 0
	for i := range pieces {
		size := base
		if i < extra {
			size++
		}
		if size == 0 {
			continue
		}
		start += size
		bounds = append(bounds, start)
	}
	if len(bounds) == 1 {
		bounds = append(bounds, items)
	}
	return bounds
}

// ShardBytes cuts sizes into calls of at most maxBytes, runs at most workers at a time, and hands fn
// each piece's bounds.
func ShardBytes[R any](sizes []int, maxBytes, workers int, fn func(start, end int) R) []R {
	bounds := ChunkBounds(sizes, maxBytes, workers)
	pieces := len(bounds) - 1
	results := make([]R, pieces)
	if workers <= 1 || pieces == 1 {
		// A worker running these serially would only add a handoff to the caller's own goroutine.
		for i := range results {
			results[i] = fn(bounds[i], bounds[i+1])
		}
		return results
	}
	// Every piece is buffered before a worker exists, so the close only says when to stop.
	queue := make(chan int, pieces)
	for i := range pieces {
		queue <- i
	}
	close(queue)
	var wg sync.WaitGroup
	for range min(workers, pieces) {
		wg.Go(func() {
			for i := range queue {
				results[i] = fn(bounds[i], bounds[i+1])
			}
		})
	}
	wg.Wait()
	return results
}
