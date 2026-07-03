package controller

import (
	"context"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/harishhary/blink/internal/config"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/snapshot"
)

const (
	watchDebounce = 400 * time.Millisecond
	// watchPoll is a periodic fallback: on macOS/kqueue, REMOVE events may not fire while a
	// process holds the file descriptor open.
	watchPoll = 5 * time.Second
)

// parsedFile caches a parsed sidecar with the stat (size+mtime pre-gate) and FNV-1a content hash
// used to skip re-reading/re-parsing unchanged files.
type parsedFile[T plugin.Syncable] struct {
	item    T
	hash    uint64
	size    int64
	modTime time.Time
}

// binaryFile caches an artifact binary's sha256 digest with the same stat pre-gate (skips re-hashing
// unchanged binaries; a full sha256 per poll would be wasteful).
type binaryFile struct {
	hash    string // sha256 hex (== helpers.BinaryChecksum)
	size    int64
	modTime time.Time
}

// LocalReader is the disk-side twin of SnapshotReader: it assembles a *snapshot.Snapshot from a
// directory of YAML sidecars (parse → elect → publish). Election is incremental and fail-safe (a bad
// parse keeps the last-good entry, never tombstoning a running plugin).
type LocalReader[T plugin.Syncable] struct {
	logger   *logger.Logger
	name     string                            // plugin-type label used in logs
	dir      string                            // sidecar directory to watch
	loader   config.Loader[T]                  // parses one YAML sidecar into T
	snapshot atomic.Pointer[snapshot.Snapshot] // elected snapshot, wait-free (for Snapshot())
	ready    atomic.Bool                       // true once the first parse+election has completed

	// mu serialises the whole rebuild (parse + election) and guards the state it mutates -
	// cache/entries - plus localRevision and the watcher set. Snapshot()/Ready() are
	// wait-free (atomic) and never take it, so publishing never blocks a reader; only the cold
	// paths IDs()/Subscribe() contend here.
	mu            sync.Mutex
	entries       map[string]snapshot.EffectiveEntry // logical ID → elected entry; source of truth for the snapshot
	localRevision int64                              // per-pod change token, published as Snapshot.Generation (not the DB generation)
	watchers      map[chan<- struct{}]struct{}

	cache    map[string]parsedFile[T] // path → last-good parsed item + content hash; retained on parse error (fail-safe)
	binaries map[string]binaryFile    // binary stem → digest + stat; election stamps these onto each ArtifactRef
}

// NewLocalReader builds a LocalReader that watches dir for YAML sidecars (parsed by loader).
// name is the short plugin-type label used in logs.
func NewLocalReader[T plugin.Syncable](log *logger.Logger, name, dir string, loader config.Loader[T]) *LocalReader[T] {
	r := &LocalReader[T]{
		logger:   log,
		name:     name,
		dir:      dir,
		loader:   loader,
		cache:    make(map[string]parsedFile[T]),
		binaries: make(map[string]binaryFile),
		entries:  make(map[string]snapshot.EffectiveEntry),
		watchers: make(map[chan<- struct{}]struct{}),
	}
	return r
}

// IDs returns the distinct logical plugin IDs currently present in the directory - including groups
// election would drop as invalid - for PluginController's Postgres reconciliation and carry-forward.
// This is a cold-path read (controller reconcile, on config change only), so it reads under mu
// rather than maintaining a wait-free copy the way Snapshot() does for the hot path.
func (r *LocalReader[T]) IDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]struct{}, len(r.cache))
	for _, pf := range r.cache {
		seen[pf.item.Metadata().Id] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids
}

// Snapshot returns the latest elected snapshot, or nil before the first election. Wait-free;
// satisfies plugin.SnapshotSource.
func (r *LocalReader[T]) Snapshot() *snapshot.Snapshot { return r.snapshot.Load() }

// Ready reports whether the first parse+election has completed - the dev-mode /health/ready gate.
func (r *LocalReader[T]) Ready() bool { return r.ready.Load() }

// Subscribe returns a cap-1 change channel plus an unsubscribe func; signals coalesce, so
// always re-read Snapshot() (and IDs(), from the controller) on receipt.
func (r *LocalReader[T]) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	send := (chan<- struct{})(ch)
	r.mu.Lock()
	r.watchers[send] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.watchers, send)
		r.mu.Unlock()
	}
}

// Start does an initial parse+election, then runs the shared watch loop, re-parsing on each change
// until ctx is cancelled. rebuild is the disk analogue of SnapshotReader's Start-loop body: reelect
// mutates the entries map (parse + election) and, on an actual change, rebuildAndSignal publishes the
// new snapshot. It has no callers outside Start, so it lives here as a closure rather than a method.
// Both the initial call and the watch-loop callback run it serially, so elections never overlap.
func (r *LocalReader[T]) Start(ctx context.Context) error {
	rebuild := func() {
		if r.reelect() {
			r.rebuildAndSignal()
			r.ready.Store(true)
		}
	}
	rebuild()

	// Set up the fsnotify watcher synchronously so a setup failure surfaces from Start; the
	// debounce/poll loop then runs in a goroutine (runWatchLoop).
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := fsw.Add(r.dir); err != nil {
		fsw.Close()
		return err
	}
	go runWatchLoop(ctx, fsw, r.logger, func(string) { rebuild() })
	return nil
}

// parse scans dir and updates the cache, returning the paths that were (re)parsed, the paths that
// were deleted, the executable (binary) names, binChanged (binary stems whose digest changed or that
// were removed, so the owning group re-elects), and oldIDs - the pre-change logical ID of every
// changed/deleted path that had a prior cache entry, so rebuild can re-elect the group a moved or
// removed file left behind (the id is read off cache before it is overwritten/deleted). It is
// fail-safe: a file that fails to read/parse (or a readdir failure) keeps its last-good cache entry
// rather than vanishing. A cheap (size, mtime) stat pre-gate skips unchanged files without reading
// them; on a stat change the content hash decides whether it actually changed. Caller holds mu.
func (r *LocalReader[T]) parse() (changed, deleted, binaries, binChanged []string, oldIDs map[string]string) {
	oldIDs = make(map[string]string)
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		r.logger.ErrorF("local-reader: readdir %s: %v", r.dir, err)
		return nil, nil, nil, nil, oldIDs // keep last-good; report no changes
	}
	seen := make(map[string]struct{}, len(entries))
	seenBins := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode()&0111 != 0 {
			name := e.Name()
			binaries = append(binaries, name)
			seenBins[name] = struct{}{}
			// Same (size, mtime) stat pre-gate as YAML - more valuable here since binaries are large.
			size, mtime := info.Size(), info.ModTime()
			if prev, ok := r.binaries[name]; ok && prev.size == size && prev.modTime.Equal(mtime) {
				continue
			}
			digest, err := helpers.BinaryChecksum(filepath.Join(r.dir, name))
			if err != nil {
				r.logger.ErrorF("local-reader: checksum binary %s: %v (keeping last-good)", name, err)
				continue // fail-safe: keep the last-good digest, retry next scan
			}
			if prev, ok := r.binaries[name]; ok && prev.hash == digest {
				prev.size, prev.modTime = size, mtime // content identical (e.g. touch): refresh stat only
				r.binaries[name] = prev
				continue
			}
			r.binaries[name] = binaryFile{hash: digest, size: size, modTime: mtime}
			binChanged = append(binChanged, name)
			continue
		}
		if !isYAML(e.Name()) {
			continue
		}
		path := filepath.Join(r.dir, e.Name())
		seen[path] = struct{}{}

		// Cheap stat pre-gate: if size and mtime both match the last successful parse, assume the
		// content is unchanged and skip the read entirely (keeps the steady-state poll to a stat per
		// file). Blind spot: an edit preserving both size and mtime is missed until the next change.
		size, mtime := info.Size(), info.ModTime()
		if prev, ok := r.cache[path]; ok && prev.size == size && prev.modTime.Equal(mtime) {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			r.logger.ErrorF("local-reader: read %s: %v (keeping last-good)", e.Name(), err)
			continue // fail-safe: cache[path] retained so we retry next scan
		}
		h := hashBytes(data)
		if prev, ok := r.cache[path]; ok && prev.hash == h {
			// Stat changed but the content is byte-identical (e.g. a touch): refresh the stat so we
			// don't re-read next scan, but don't re-parse - nothing actually changed.
			prev.size, prev.modTime = size, mtime
			r.cache[path] = prev
			continue
		}

		stem := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		item, err := r.loader.ParseSpec(stem, data)
		if err != nil {
			r.logger.ErrorF("local-reader: parse %s: %v (keeping last-good)", e.Name(), err)
			continue // fail-safe: cache[path] retained, stat not advanced so we retry
		}
		if prev, ok := r.cache[path]; ok {
			oldIDs[path] = prev.item.Metadata().Id // capture the pre-change id before the overwrite
		}
		r.cache[path] = parsedFile[T]{item: item, hash: h, size: size, modTime: mtime}
		changed = append(changed, path)
	}
	for path, pf := range r.cache {
		if _, ok := seen[path]; !ok {
			oldIDs[path] = pf.item.Metadata().Id // capture the id before the delete
			delete(r.cache, path)
			deleted = append(deleted, path)
		}
	}
	for name := range r.binaries {
		if _, ok := seenBins[name]; !ok {
			delete(r.binaries, name)
			binChanged = append(binChanged, name) // the group owning this artifact loses its digest
		}
	}
	return changed, deleted, binaries, binChanged, oldIDs
}

// reelect reparses the catalog and re-elects only the logical IDs whose files changed, updating the
// entries map. It reports whether the published snapshot should be rebuilt: true on the first pass
// or whenever an elected entry actually changed. This is the disk-side analogue of
// SnapshotReader.apply (mutate the entries map; the caller then publishes).
func (r *LocalReader[T]) reelect() (publish bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	changed, deleted, binaries, binChanged, oldIDs := r.parse()
	all := r.cachedItems() // full catalog, for the wholesale advisory validation + first-pass election

	first := !r.ready.Load()
	if len(changed) == 0 && len(deleted) == 0 && len(binChanged) == 0 && !first {
		return false // nothing changed on disk
	}

	// Advisory validation (semver, missing fields, binary-without-yaml, ...): logged, non-blocking,
	// over the whole catalog (cheap - no marshal). The config still publishes; group-policy election
	// and the controller's carry-forward decide what actually runs.
	for _, ve := range r.loader.Validate(all, binaries) {
		r.logger.ErrorF("local-reader: %s validate: %v", r.name, ve)
	}

	// Affected IDs: the old ID of every changed/deleted path (that group lost/changed a file) plus
	// the new ID of every changed path (that group gained/changed one). An id-change touches both.
	// On the first pass every ID is affected.
	affected := make(map[string]struct{})
	if first {
		for _, it := range all {
			affected[it.Metadata().Id] = struct{}{}
		}
	}
	for _, p := range changed {
		if oldID, ok := oldIDs[p]; ok {
			affected[oldID] = struct{}{}
		}
		affected[r.cache[p].item.Metadata().Id] = struct{}{}
	}
	for _, p := range deleted {
		if oldID, ok := oldIDs[p]; ok {
			affected[oldID] = struct{}{}
		}
	}
	// A changed or removed binary affects the group of the artifact whose stem it is.
	if len(binChanged) > 0 {
		nameToID := make(map[string]string, len(r.cache))
		for _, pf := range r.cache {
			m := pf.item.Metadata()
			nameToID[m.Name] = m.Id
		}
		for _, stem := range binChanged {
			if id, ok := nameToID[stem]; ok {
				affected[id] = struct{}{}
			}
		}
	}

	// Gather the items for each affected ID (one cache scan, collecting only affected groups).
	byID := make(map[string][]T, len(affected))
	for _, pf := range r.cache {
		id := pf.item.Metadata().Id
		if _, ok := affected[id]; ok {
			byID[id] = append(byID[id], pf.item)
		}
	}

	// Binary digests keyed by artifact stem, stamped onto each elected ArtifactRef so a binary
	// change (not just a YAML edit) flows through the snapshot.
	digests := make(map[string]string, len(r.binaries))
	for name, bf := range r.binaries {
		digests[name] = bf.hash
	}

	// Re-elect each affected group; update the entries map. Only these groups pay the marshal cost.
	entriesChanged := false
	for id := range affected {
		// Each enabled artifact must be a (yaml, binary) pair: an enabled artifact with no binary is
		// invalid and dropped (honouring the loader's blocking pairing rule). A disabled artifact
		// needs no binary - it will not run. Dropping the candidate leaves the primary running;
		// dropping the primary leaves no baseline, so validateGroup then drops the whole plugin.
		var paired []T
		for _, item := range byID[id] {
			m := item.Metadata()
			if m.Enabled && digests[m.Name] == "" {
				r.logger.ErrorF("local-reader: %q (id %q) enabled but has no binary, dropped (unpaired)", m.Name, id)
				continue
			}
			paired = append(paired, item)
		}
		group := CatalogGroup[T]{ID: id, Entries: paired}
		if len(group.Entries) == 0 { // every artifact for this ID is gone or unpaired
			if _, ok := r.entries[id]; ok {
				delete(r.entries, id)
				entriesChanged = true
			}
			continue
		}
		if errs := validateGroup(group); len(errs) > 0 {
			for _, e := range errs {
				r.logger.ErrorF("local-reader: group %q invalid, dropped: %s", id, e)
			}
			if _, ok := r.entries[id]; ok { // was valid, now invalid → drop (controller carries forward)
				delete(r.entries, id)
				entriesChanged = true
			}
			continue
		}
		e := electGroup(id, group, digests)
		if old, ok := r.entries[id]; !ok || !entryEqual(old, e) {
			r.entries[id] = e
			entriesChanged = true
		}
	}

	// Publish on the first election or whenever an elected entry actually changed; a comment-only
	// edit re-elects to an identical result and is suppressed here.
	return first || entriesChanged
}

// rebuildAndSignal assembles the entries map into a fresh sorted Snapshot (bumping localRevision so
// SnapshotConfig's per-generation cache invalidates), publishes it wait-free, and fans out to
// watchers. Identical in shape to SnapshotReader.rebuildAndSignal.
func (r *LocalReader[T]) rebuildAndSignal() {
	r.mu.Lock()
	defer r.mu.Unlock()

	next := make([]snapshot.EffectiveEntry, 0, len(r.entries))
	for _, e := range r.entries {
		next = append(next, e)
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Id < next[j].Id })

	r.localRevision++
	r.snapshot.Store(&snapshot.Snapshot{Generation: r.localRevision, Entries: next})
	for ch := range r.watchers {
		select {
		case ch <- struct{}{}:
		default: // watcher already has a pending signal; it will re-read the latest pointer
		}
	}
	r.logger.Info("local-reader: elected snapshot (generation=%d, entries=%d)", r.localRevision, len(next))
}

// runWatchLoop drives fsw with a debounce + periodic-poll fallback, calling onChange(reason) on
// each change until ctx is cancelled, then closes fsw. LocalReader.Start creates the watcher and
// launches this in a goroutine (the data-plane executors get their changes from Kafka instead).
func runWatchLoop(ctx context.Context, fsw *fsnotify.Watcher, log *logger.Logger, onChange func(reason string)) {
	defer fsw.Close()
	var timer *time.Timer
	poll := time.NewTicker(watchPoll)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-fsw.Events:
			if !ok {
				return
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(watchDebounce, func() { onChange("debounce") })
		case <-poll.C:
			onChange("poll")
		case err := <-fsw.Errors:
			log.ErrorF("fsnotify error: %v", err)
			onChange("overflow")
		}
	}
}

func (r *LocalReader[T]) cachedItems() []T {
	items := make([]T, 0, len(r.cache))
	for _, v := range r.cache {
		items = append(items, v.item)
	}
	return items
}

// hashBytes returns the FNV-1a 64-bit hash of b, used to detect content changes between scans.
func hashBytes(b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}

// isYAML reports whether name is a YAML sidecar (by extension).
func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
