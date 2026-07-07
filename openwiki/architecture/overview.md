# Architecture Overview

Blink separates **what should run** (control plane) from **what actually runs**
(data plane), joined by Kafka. This page explains that split, the pipeline wiring,
and the generic plugin machinery. For the deep, authoritative treatment, follow the
links into [`/docs/internals/`](../../docs/internals/README.md) and
[`/docs/services/`](../../docs/services/README.md); this page is the map.

## The two planes

```
   YAML plugin configs (control plane only)
          │
          ▼
  ┌------------------┐  Snapshot  ┌-----------------------┐
  │   LocalReader[T] │----------►│   PluginController[T]  │   control plane
  │  parse + elect    │            │  reconcile ↔ Database │   (cmd/controller,
  │  → Snapshot       │            │  publish Snapshot     │    single writer)
  └------------------┘            └-----------┬-----------┘
                                              │ protobuf snapshot
                                              ▼
                                  KAFKA  <type>_SNAPSHOT topic
                                  (per-ID keyed, log-compacted, broadcast)
- - - - - - - - - - - - - - - - - - - - - - -│- - - per pipeline pod - - - - - -
                                              ▼
                                  ┌------------------┐
                                  │  SnapshotReader  │  wait-free cache + Subscribe()
                                  └---┬----------┬---┘
                          Snapshot()  │          │ Snapshot() + change signal
                                      ▼          ▼
                       ┌------------------┐   ┌-----------------------┐
                       │ SnapshotConfig[T]│   │   PluginExecutor[T]   │   data plane
                       │  desired config  │◄--│  spawn/ping/restart   │
                       └--------┬---------┘   └-----------┬-----------┘
                                │                         │ notify(Register/Update/…)
                                ▼                         ▼
                              rollout            ┌-----------------------┐
                              closure ----------►│   ProcessPool[T]      │
                                                 │   VersionedPool[T]    │
                                                 │   Call() routes by    │
                                                 │   rollout mode        │
                                                 └-----------------------┘
```

- **Control plane** is one binary, `cmd/controller`, running one
  `PluginController[T]` per plugin type. It reads YAML plugin configs, reconciles
  them against a persistence `Database`, and publishes an effective `Snapshot` to
  that type's Kafka snapshot topic. It is the **only writer of desired state**, so it
  validates rollout policy once and ships a vetted snapshot.
- **Data plane** is the seven pipeline services. Each pod runs a `SnapshotReader`
  (per-pod read replica of the snapshot topic), a `PluginExecutor` that reconciles
  subprocesses against the snapshot, and the stage's own transform loop. **No
  data-plane pod reads plugin config off disk** — the plugin directory is only a
  binary artifact store.

Key generic types (all instantiated once per plugin type):

| Type                  | File                                     | Responsibility                                                                                      |
| --------------------- | ---------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `LocalReader[T]`      | `internal/controller/local_reader.go`    | Watch YAML, parse + run rollout election → `*snapshot.Snapshot`. Controller only.                   |
| `PluginController[T]` | `internal/controller/controller.go`      | Reconcile YAML ↔ `Database`, compute & publish the effective `Snapshot`.                            |
| `SnapshotReader`      | `internal/controller/snapshot_reader.go` | Per-pod read replica: consume snapshot topic, cache, signal subscribers.                            |
| `SnapshotConfig[T]`   | `internal/config/snapshot_config.go`     | The data plane's `config.Source[T]`: rebuild specs from the snapshot.                               |
| `PluginExecutor[T]`   | `internal/plugin/executor.go`            | Reconcile subprocesses against the snapshot; spawn/restart; emit bus messages.                      |
| `ProcessPool[T]`      | `internal/pools/pool.go`                 | Hold `VersionedPool`s keyed by `PoolKey{Id,Name,Hash}`; route calls by rollout mode; drain on swap. |
| `VersionedPool[T]`    | `internal/pools/versioned_pool.go`       | Fixed-size semaphore over one binary version's subprocess handles.                                  |

Reference: [`/docs/internals/README.md`](../../docs/internals/README.md) lists the
recommended reading order for these types.

## The reconcile loop (shared by both planes)

Both planes run the same pattern: **subscribe → coalescing signal → re-read → re-sync**.

- Control plane: `LocalReader` (source) → `PluginController` (subscriber).
- Data plane: `SnapshotReader` (source) → `PluginExecutor` (subscriber).

A source assembles the latest `Snapshot` and fires a *coalescing* change signal; the
single subscriber re-reads the latest snapshot and re-syncs. The "many readers"
scale-out is the Kafka broadcast across pods, not multiple subscribers per source.
Authoritative walkthrough:
[`/docs/internals/reconcile-loop.md`](../../docs/internals/reconcile-loop.md).

## The pipeline (data plane)

Seven stages, each consuming one Kafka topic and writing the next:

| Stage              | Plugin type  | Reference                                                                       |
| ------------------ | ------------ | ------------------------------------------------------------------------------- |
| `event_matcher`    | matchers     | [`/docs/services/event_matcher.md`](../../docs/services/event_matcher.md)       |
| `rule_executor`    | rules        | [`/docs/services/rule_executor.md`](../../docs/services/rule_executor.md)       |
| `alert_merger`     | _(none)_     | [`/docs/services/alert_merger.md`](../../docs/services/alert_merger.md)         |
| `rule_tuner`       | tuning_rules | [`/docs/services/rule_tuner.md`](../../docs/services/rule_tuner.md)             |
| `alert_enricher`   | enrichments  | [`/docs/services/alert_enricher.md`](../../docs/services/alert_enricher.md)     |
| `alert_formatter`  | formatters   | [`/docs/services/alert_formatter.md`](../../docs/services/alert_formatter.md)   |
| `alert_dispatcher` | _(sink)_     | [`/docs/services/alert_dispatcher.md`](../../docs/services/alert_dispatcher.md) |

### The shared service skeleton

Every data-plane service is structurally identical. `Run` is always: pull a Kafka
batch → `processBatch` → commit offsets. `processBatch` follows one pattern:

1. **Decode** each message into events/alerts.
2. **Group by plugin** — build `map[pluginName][]index` so each plugin subprocess is
   called *once per batch* with all relevant items, not once per item. This collapses
   the gRPC call count dramatically.
3. **Fan out** — one goroutine per plugin, results merged under a mutex; the executor
   additionally bounds concurrency with a `semaphore.Weighted`.
4. **Write** to the next topic (with a DLQ fallback in tuner/enricher/formatter).

Services are wired together by `internal/services/runner.go` (`RegisterInit` runs
first to completion; `Register` runs concurrently under exponential-backoff restart).
Authoritative overview: [`/docs/services/README.md`](../../docs/services/README.md).

## Transport and persistence

- **Brokers** (`internal/brokers`) abstract Kafka behind `Reader`/`Writer`/`Broker`
  interfaces (`broker.go`), with a Kafka implementation (`kafka.go`) and an Azure
  Event Hubs implementation (`eventhub.go`). Note the distinction between a
  consumer-group `NewReader` (work queue, partition split) and `NewBroadcastReader`
  (every reader sees every message — used for the single-partition, log-compacted
  snapshot topics).
- **Backends** (`internal/backends`) are the control-plane persistence layer behind a
  `Database` interface (`database.go`): `sql.go` (SQLite/Postgres) and `nodb.go` (a
  no-op). The controller uses it for bootstrap, generation tracking, and snapshot
  carry-forward. Historical cloud backends (Athena, Snowflake, DynamoDB, Elastic)
  were removed in the recent refactor.

## Where to make changes

- **Reconcile/lifecycle/rollout logic** → `internal/controller`, `internal/plugin`,
  `internal/pools`. Read the relevant `/docs/internals/*.md` first; these areas have
  extensive table-driven tests (e.g. `internal/pools/pool_*_test.go`).
- **A pipeline stage's transform** → `cmd/<service>/` plus its `*/<subpkg>/` logic
  package (e.g. `cmd/rule_executor/executor/`). Check the matching `/docs/services`
  page for the message contract.
- **Transport or persistence** → `internal/brokers` / `internal/backends`; keep the
  interface stable so the controller and services don't need to change.

See also: [architecture/plugins-and-rollout.md](plugins-and-rollout.md) and
[domain/model.md](../domain/model.md).
