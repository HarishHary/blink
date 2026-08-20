package runtime

import "sync"

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

func ShardConcurrent[T, R any](items []T, maxChunks int, fn func([]T) R) []R {
	if maxChunks > len(items) {
		maxChunks = len(items)
	}
	if maxChunks <= 1 {
		return []R{fn(items)}
	}
	chunks := ShardSlice(items, maxChunks)
	results := make([]R, len(chunks))
	var wg sync.WaitGroup
	for i := range chunks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = fn(chunks[i])
		}()
	}
	wg.Wait()
	return results
}
