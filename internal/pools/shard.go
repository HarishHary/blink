package pools

import "sync"

// ShardSlice splits xs into k contiguous, near-equal chunks (k >= 1), order-preserving. The first
// (len(xs) % k) chunks get one extra element; empty chunks are omitted, so concatenating per-chunk
// results realigns 1:1 with xs. Callers use it to fan a batch across a plugin's max_procs workers.
func ShardSlice[E any](xs []E, k int) [][]E {
	if k < 1 {
		k = 1
	}
	n := len(xs)
	base := n / k
	rem := n % k
	chunks := make([][]E, 0, k)
	start := 0
	for i := 0; i < k; i++ {
		size := base
		if i < rem {
			size++
		}
		if size == 0 {
			continue
		}
		chunks = append(chunks, xs[start:start+size])
		start += size
	}
	return chunks
}

// ShardConcurrent splits xs into up to k contiguous chunks and runs fn on each concurrently, returning
// the per-chunk results in chunk order (out[i] is fn(chunk i), and chunk i precedes chunk i+1, so a
// caller that concatenates the per-chunk results stays aligned with xs). With k <= 1 (or len(xs) <= 1)
// it runs fn once over xs - the un-sharded path. k is clamped to len(xs) so no empty chunk is created.
func ShardConcurrent[E, R any](xs []E, k int, fn func([]E) R) []R {
	if k > len(xs) {
		k = len(xs)
	}
	if k <= 1 {
		return []R{fn(xs)}
	}
	chunks := ShardSlice(xs, k)
	out := make([]R, len(chunks))
	var wg sync.WaitGroup
	for i := range chunks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = fn(chunks[i])
		}(i)
	}
	wg.Wait()
	return out
}
