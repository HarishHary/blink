package controller

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/harishhary/blink/internal/brokers"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/snapshot"
)

// catchUpPoll bounds how long a not-yet-ready reader blocks on a single read before checking
// whether it has drained the cold-start backlog. Short enough that an idle/empty topic goes
// ready quickly; only used until Ready, after which reads block normally.
const catchUpPoll = 250 * time.Millisecond

// snapshotUnmarshalErrors counts undeserializable snapshot messages (schema mismatch/corruption) -
// the broadcast-topic analogue of a DLQ alert (nothing to retry; non-zero = possibly stale config).
var snapshotUnmarshalErrors = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "blink",
	Subsystem: "snapshot_reader",
	Name:      "unmarshal_errors_total",
	Help:      "Snapshot messages that failed to deserialize (schema mismatch or corruption).",
})

// snapshotAppliedGeneration is the controller DB generation this pod has consumed; min() across pods
// vs the controller's generation confirms a rollout reached the fleet (assumes one reader per process).
var snapshotAppliedGeneration = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: "blink",
	Subsystem: "snapshot_reader",
	Name:      "applied_generation",
	Help:      "Controller DB generation this pod has consumed from the snapshot topic (rollout tracking).",
})

// SnapshotReader is the read end of the controller's one-writer/many-readers split: it consumes the
// per-ID keyed, log-compacted topic (upserts + tombstones), assembles the desired-state Snapshot, and
// signals watchers on change. A cold reader replays the compacted set and goes Ready once drained (ReadLag()==0).
type SnapshotReader struct {
	logger     *logger.Logger
	reader     brokers.Reader
	snapshot   atomic.Pointer[snapshot.Snapshot] // assembled view, wait-free for readers
	ready      atomic.Bool                       // true once the cold-start backlog is drained
	appliedGen atomic.Int64                      // latest DB generation marker consumed; wait-free (AppliedGeneration)

	mu            sync.Mutex
	entries       map[string]snapshot.EffectiveEntry // by logical plugin ID; the source of truth
	localRevision int64                              // per-pod cache token, published as Snapshot.Generation (not the DB generation)
	watchers      map[chan<- struct{}]struct{}
}

// NewSnapshotReader creates a reader that consumes the per-ID keyed snapshot topic from
// reader (bound to the controller's snapshot topic) once Start is called.
func NewSnapshotReader(logger *logger.Logger, reader brokers.Reader) *SnapshotReader {
	return &SnapshotReader{
		logger:   logger,
		reader:   reader,
		entries:  make(map[string]snapshot.EffectiveEntry),
		watchers: make(map[chan<- struct{}]struct{}),
	}
}

// Snapshot returns the latest assembled snapshot, or nil before the reader is Ready.
// Wait-free; safe for concurrent use.
func (r *SnapshotReader) Snapshot() *snapshot.Snapshot { return r.snapshot.Load() }

// Ready reports whether the cold-start backlog is drained - false until the keyed set is fully replayed.
// A pipeline pod gates /health/ready on this so it stays out of rotation until fully configured.
func (r *SnapshotReader) Ready() bool { return r.ready.Load() }

// AppliedGeneration returns the controller DB generation this pod has consumed (latest marker read, 0 if
// none). The marker is written last, so a marker for N is seen only after all of N's entries apply - the
// value is always backed by fully-applied state. Compare min() across pods to confirm a rollout. Wait-free.
func (r *SnapshotReader) AppliedGeneration() int64 { return r.appliedGen.Load() }

// Subscribe returns a cap-1 change channel plus an unsubscribe func; signals coalesce, so always
// re-read Snapshot() on receipt.
func (r *SnapshotReader) Subscribe() (<-chan struct{}, func()) {
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

// Start consumes keyed snapshot messages, applies each (upsert or tombstone) to the assembled
// state, and signals watchers. Runs until ctx is cancelled. Satisfies services.Manager, so it
// can be run as a service via services.NewManagedService.
func (r *SnapshotReader) Start(ctx context.Context) error {
	const baseBackoff, maxBackoff = 100 * time.Millisecond, 5 * time.Second
	backoff := baseBackoff
	for {
		// Until ready, read with a timeout so a drained backlog (lull) is observable; after ready, block normally.
		readCtx := ctx
		cancel := context.CancelFunc(func() {}) // no-op default; replaced when we add a timeout
		if !r.ready.Load() {
			readCtx, cancel = context.WithTimeout(ctx, catchUpPoll)
		}
		msg, err := r.reader.ReadMessage(readCtx)
		cancel()

		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			if !r.ready.Load() && errors.Is(err, context.DeadlineExceeded) {
				r.checkCaughtUp(ctx) // lull: confirm ReadLag()==0 and go ready
				continue
			}
			// Bounded, ctx-aware backoff so a persistent read error doesn't spin the CPU or flood logs; reset on success.
			r.logger.ErrorF("snapshot-reader: kafka read: %v (retry in %v)", err, backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = baseBackoff // successful read clears the penalty

		if !r.apply(msg) {
			continue // unmarshal error or generation marker; nothing to publish
		}
		// While catching up we only mutate the map (assembling the sorted Snapshot per message
		// would be O(n²) over a large backlog); the snapshot is built once on the ready
		// transition. After ready, reflect each change immediately.
		if r.ready.Load() {
			r.rebuildAndSignal()
		}
	}
}

// apply applies one keyed message to the assembled-state map and reports whether the entry set changed
// (caller then rebuild+signals): a generation marker updates AppliedGeneration (returns false), a nil/empty
// value tombstones the ID, otherwise the value is an EffectiveEntry to upsert. Unmarshal error → false.
func (r *SnapshotReader) apply(msg brokers.Message) bool {
	key := string(msg.Key)
	if key == snapshot.GenerationMarkerKey {
		gen, err := snapshot.DecodeGeneration(msg.Value)
		if err != nil {
			snapshotUnmarshalErrors.Inc()
			r.logger.ErrorF("snapshot-reader: decode generation marker: %v", err)
			return false
		}
		if prev := r.appliedGen.Swap(gen); gen < prev {
			// Monotonic on a single-partition topic, so a backwards value means a controller rollback
			// (DB restore) or split-brain. We still accept it as authoritative (the republished entries
			// are that older state too) but surface the anomaly.
			r.logger.ErrorF("snapshot-reader: generation went backwards %d → %d (controller rollback or split-brain?)", prev, gen)
		}
		snapshotAppliedGeneration.Set(float64(gen))
		return false // marker carries no entry; nothing to rebuild/signal
	}
	if len(msg.Value) == 0 {
		r.mu.Lock()
		delete(r.entries, key)
		r.mu.Unlock()
		return true
	}
	e, err := snapshot.Unmarshal(msg.Value)
	if err != nil {
		snapshotUnmarshalErrors.Inc()
		r.logger.ErrorF("snapshot-reader: unmarshal entry %q: %v", key, err)
		return false
	}
	r.mu.Lock()
	r.entries[e.Id] = e
	r.mu.Unlock()
	return true
}

// checkCaughtUp confirms the backlog is drained (ReadLag()==0) and, if so, assembles the first snapshot,
// flips Ready, and signals watchers. Lag round-trips to the broker, so it's called only on a read lull.
func (r *SnapshotReader) checkCaughtUp(ctx context.Context) {
	lag, err := r.reader.ReadLag(ctx)
	if err != nil {
		r.logger.ErrorF("snapshot-reader: lag: %v", err)
		return
	}
	if lag > 0 {
		return // more backlog to drain
	}
	r.rebuildAndSignal()
	r.ready.Store(true)
	r.logger.Info("snapshot-reader: caught up (entries=%d)", len(r.entries))
}

// rebuildAndSignal assembles the map into a fresh sorted Snapshot (bumping localRevision so
// SnapshotConfig's per-generation cache invalidates), publishes it wait-free, and fans out to watchers.
func (r *SnapshotReader) rebuildAndSignal() {
	r.mu.Lock()
	entries := make([]snapshot.EffectiveEntry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Id < entries[j].Id })
	r.localRevision++
	r.snapshot.Store(&snapshot.Snapshot{Generation: r.localRevision, Entries: entries})
	for ch := range r.watchers {
		select {
		case ch <- struct{}{}:
		default: // watcher already has a pending signal; it will re-read the latest pointer
		}
	}
	r.mu.Unlock()
}
