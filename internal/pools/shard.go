package pools

import "sync"

// ShardSlice splits items into maxChunks contiguous, near-equal chunks (maxChunks >= 1),
// order-preserving. The first (len(items) % maxChunks) chunks get one extra element; empty chunks
// are omitted, so concatenating per-chunk results realigns 1:1 with items. Callers use it to fan a
// batch across a plugin's max_procs workers.
func ShardSlice[Item any](items []Item, maxChunks int) [][]Item {
	if maxChunks < 1 {
		maxChunks = 1
	}
	itemCount := len(items)
	baseChunkSize := itemCount / maxChunks
	extraItems := itemCount % maxChunks
	chunks := make([][]Item, 0, maxChunks)
	startIndex := 0
	for chunkIndex := 0; chunkIndex < maxChunks; chunkIndex++ {
		chunkSize := baseChunkSize
		if chunkIndex < extraItems {
			chunkSize++
		}
		if chunkSize == 0 {
			continue
		}
		chunks = append(chunks, items[startIndex:startIndex+chunkSize])
		startIndex += chunkSize
	}
	return chunks
}

// ShardConcurrent splits items into up to maxChunks contiguous chunks and runs processChunk on each
// concurrently, returning the per-chunk results in chunk order (results[i] is processChunk(chunk i),
// and chunk i precedes chunk i+1, so a caller that concatenates the per-chunk results stays aligned
// with items). With maxChunks <= 1 (or len(items) <= 1) it runs processChunk once over items - the
// un-sharded path. maxChunks is clamped to len(items) so no empty chunk is created.
func ShardConcurrent[Item, Result any](items []Item, maxChunks int, processChunk func([]Item) Result) []Result {
	if maxChunks > len(items) {
		maxChunks = len(items)
	}
	if maxChunks <= 1 {
		return []Result{processChunk(items)}
	}
	chunks := ShardSlice(items, maxChunks)
	results := make([]Result, len(chunks))
	var wg sync.WaitGroup
	for chunkIndex := range chunks {
		wg.Add(1)
		go func(chunkIndex int) {
			defer wg.Done()
			results[chunkIndex] = processChunk(chunks[chunkIndex])
		}(chunkIndex)
	}
	wg.Wait()
	return results
}
