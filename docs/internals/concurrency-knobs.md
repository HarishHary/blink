# Concurrency knobs

[Internals index](README.md) · [Plugin runtime](plugin-runtime.md) · [Event matcher service](../services/event_matcher.md)

Every concurrency knob moves one of four quantities: how many plugin calls a batch makes, how many run at once, how many wait, and how many are refused. This page names each knob, what moving it does, and the queue that absorbs it.

## How many calls a batch makes

Nothing picks a call count. It follows from the batch:

```text
fanOut             =  min(MAX_BATCH_SIZE, 101), or 101 when unset
plugins called     =  distinct plugins the batch's own rows name
groups per plugin  =  2 while a partial canary exists, else 1
call capacity      =  min(max(1, max_procs) x calls_per_process, fanOut)
workers per group  =  max(1, call capacity / groups per plugin)
chunks for payload =  ceil(group size / (3 MiB / bytes per item))
chunks per group   =  max(workers per group, chunks for payload), capped by the group's size
calls per batch    =  plugins called x groups per plugin x chunks per group
items per call     =  batch size / (groups per plugin x chunks per group)
```

- `plugins called` is data, not a knob: `groupByMatcher` makes one entry per matcher the batch's candidate rules name, and each entry is one call per attempt.
- `groups per plugin` is the routing decisions in the batch. Each call carries one rollout key, so a batch splits two ways at most. It is one group with no candidate, with a shadow or blue-green candidate, or at `rollout_pct` 0 or 100: `runtime.RouteSides` returns no groups and the batch is sent as it stands. Grouping costs an index per item plus a gather, per plugin called.
- `workers per group` is a concurrency ceiling on a group's chunks: capacity divided across the groups, rounded down, never below 1. So a canary adds no calls, its two groups dividing the same capacity - except at a capacity of 1, where each group still gets one worker and two calls replace one.
- `chunks for payload` is a correctness floor under it. Only this term can exceed the workers, so a bounded pool runs the pieces (`runtime.ShardBytes`, every item kind).

Every call carries its items already encoded, so no call pays a conversion and a copy. `event_matcher` encodes each event once when it decodes it, `rule_executor` re-encodes its record's struct once per message, and `rule_tuner` encodes each alert once when it decodes it.

`fanOut` caps at `MaxDeploymentProcs + 1` (101). Callers ask for the smaller of `fanOut` and their deployment's capacity (`Application.CallBudget`), so capacity beyond it arrives as further calls. Payload chunks widen no budget: the pool keeps at most `workers per group` invocations alive.

## The payload ceiling

gRPC defaults a receiver to 4 MiB, and go-plugin keeps that default on both ends. An oversized request is `ResourceExhausted`: the call fails, the batch exhausts `MATCHER_MAX_ATTEMPTS`, and it dead-letters. Nothing above the transport recovers, so `runtime.MaxCallPayloadBytes` is 3 MiB.

`events.Batch` and `alerts.Batch` price each item when the batch is built - encoded length plus repeated-field framing - and `runtime.ChunkBounds` cuts before the item that would cross the budget. Prices come from the encoding, not the item's shape.

| Item                             | Bytes on the wire | Items in a 3 MiB call |
| -------------------------------- | ----------------- | --------------------- |
| Event, 3 small fields            | ~65               | ~48,000               |
| Event, 1 KB of fields            | ~1,100            | ~2,800                |
| Alert, minimal rule metadata     | ~154              | ~20,000               |
| Alert, rule with tags/references | 1-3 KB            | ~1,000-3,000          |

An alert is heavier than its event: two alerts from one rule share a pointer in memory but repeat that rule's metadata on the wire. Rules declaring tags, references, log types, and observables produce the heaviest alerts.

The term binds only for large batches at low capacity. 10,000 of the events above are 650 KB, one chunk; 50,000 are 3.3 MB, so a capacity of 1 still makes two calls; a 1,000,000-event batch is 21 calls of ~47,600 events whatever the capacity says.

## Capacity is processes times calls per process

`MaxConcurrentCallsPerProcess` - `calls_per_process` in the deployment key - is what one subprocess serves at once. A deployment's ceiling on concurrently executing invocations is `max(1, max_procs) x calls_per_process`: the manager schedules from that product, the plugin process refuses anything past it, and callers shard against it.

Nothing serializes invocations; a plugin process dispatches each call to its meta-process as a message. One plugin object serves the whole process and gRPC gives every RPC its own goroutine, so above a capacity of 1 the plugin's own maps, cursors, file handles, and clients must be concurrency-safe. The default is `32`; `MaxDeploymentCallsPerProcess` stops it at 64.

Between `min_procs` and `max_procs` the process count is derived from calls:

- A manager needs `ceil(demand / calls_per_process)` processes, clamped to `min_procs..max(1, max_procs)`. Demand is the invocations it owes: executing, dispatched, and queued.
- Growth goes to that figure in one pass, once every slot of every ready process is taken and a call is still queued. Shrinking is one process per cooldown, after `IdleTimeout` with nothing queued or dispatching, and only releases a process holding no invocation.
- Every deployment keeps its `min_procs`. Growth past those reservations draws on one process-wide `ProcessBudget`, sized `GOMAXPROCS x 2` - not `NumCPU`, which ignores the container's CPU limit.
- Each active route counts separately, so a rollout doubles a plugin's subprocess count while the candidate lives. The floor is `plugins x active routes x max(1, min_procs)`, the ceiling that floor plus the budget. Route composition is in [plugin-runtime.md](plugin-runtime.md#deployment-route).
- A manager denied a permit keeps its calls queued; the scale cooldown paces the next attempt.
- Reservations are counted, never charged. A catalog whose `min_procs` exceed the budget starts every route and logs `reserved plugin processes exceed the process budget` once.

## Every knob

| Knob                                   | Set by                                                                                | Raising it                                                                                                                    | Lowering it                                                                                          | Absorbed by                                                           |
| -------------------------------------- | ------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------- |
| `MAX_BATCH_SIZE`                       | env, unset is 50, no upper bound                                                      | More events per fetch and per state snapshot; more events per call until the payload budget binds; larger replay window       | Fewer events per commit; more fetches, projection reads, and snapshot clones per event               | Kafka fetch, then the batch's own commit boundary                     |
| `MAX_CONCURRENT_CALLS`                 | env                                                                                   | More matchers evaluated at once for one batch                                                                                 | Matchers evaluated in more waves; lower peak CPU                                                     | Service semaphore: `match` blocks before submitting                   |
| `MaxBatchSize`                         | `MAX_BATCH_SIZE`                                                                      | Raises `fanOut` until 101 caps it, so it moves a budget only for batches under 101 events                                     | Shrinks it, never below the fan-out that batch can produce                                           | Nothing - it only sizes other budgets                                 |
| `MaxConcurrentCalls`                   | `MAX_CONCURRENT_CALLS`                                                                | Raises both derived budgets, since every concurrent call carries its own fan-out                                              | Shrinks both                                                                                         | Nothing - it only sizes the budgets                                   |
| `maxOutstandingInvocations`            | derived: `perPlugin x MaxConcurrentCalls`                                             | More invocations alive across all plugins; more memory in cloned shards                                                       | Callers **block** in `Submit` until a permit frees                                                   | Blocking `semaphore.Acquire`, bounded by the caller's deadline        |
| `maxOutstandingInvocationsPerPlugin`   | derived: `fanOut x MaxConcurrentCalls`                                                | One plugin may hold more of the shared budget                                                                                 | Below the fan-out, `Submit` **fails fast** with `ErrQueueFull`; the batch retries, then dead-letters | Nothing - this one rejects                                            |
| `shadowMaxOutstandingInvocations`      | derived: `max(1, shared / 16)`                                                        | More shadow evaluation of a candidate                                                                                         | Shadow calls **dropped** with `ErrShadowDropped`                                                     | Nothing - non-blocking `TryAcquire`, by design                        |
| `QueueSize`                            | derived: `max(128, perPlugin)`                                                        | One deployment may hold more calls waiting for capacity                                                                       | Below the plugin's admission budget it moves the same rejection one layer down                       | Manager pending queue, drained FIFO as capacity frees                 |
| `min_procs` (spec)                     | artifact YAML; unset is `0`                                                           | Processes exist before the first call, so no cold start; the deployment never shrinks below it                                | `0` starts nothing until a call queues, and lets the last process stop when idle                     | First call waits for a process to start                               |
| `max_procs` (spec)                     | artifact YAML, `<= MaxDeploymentProcs` (100); unset is `1`                            | More capacity and more concurrent chunks, but only while the groups leave that capacity idle                                  | Fewer chunks in flight; less per-call overhead; never chunks larger than the payload budget          | Manager queue, since fewer processes dequeue                          |
| `calls_per_process` (spec)             | `MaxConcurrentCallsPerProcess`, `<= MaxDeploymentCallsPerProcess` (64); unset is `32` | One subprocess serves that many invocations at once; safe only where the plugin's own code is concurrency-safe                | `1` serializes one call per subprocess, for a plugin that is not concurrency-safe                    | Manager queue, then the process, which refuses anything past capacity |
| `MaxCallPayloadBytes`                  | constant `3 << 20`, under the gRPC 4 MiB limit                                        | Not tunable without raising the transport limit on both ends                                                                  | Smaller chunks, more of them, unchanged concurrency                                                  | Nothing - it changes chunk count, not admission                       |
| `ProcessBudget`                        | derived `GOMAXPROCS x 2`                                                              | More subprocesses past every `min_procs`, so bursts scale further                                                             | Scale-up stops sooner, closer to the reservations                                                    | Manager queue, paced by the scale cooldown                            |
| `rollout_pct` (spec)                   | artifact YAML                                                                         | More rollout buckets routed to the canary; `0` and `100` route everything alike, so neither splits a batch                    | Fewer; `0` leaves the canary idle                                                                    | Nothing - it only picks a route                                       |
| presence of a canary candidate         | rollout state, not config                                                             | Cuts every batch for that plugin in two for the length of the rollout                                                         | With none, one group per batch                                                                       | Nothing - `RolloutByID` decides it per generation                     |
| `RolloutBucketCount`                   | constant `100`                                                                        | Not tunable; the hash space `rollout_pct` is a percentage of, so it decides which side an item lands on, never how many sides | -                                                                                                    | -                                                                     |
| `ScaleCooldown`                        | default 1s                                                                            | One scaling decision per cooldown, slower in both directions                                                                  | Faster ramp and shrink, more subprocess churn                                                        | Manager queue during the ramp                                         |
| `IdleTimeout`                          | default 30s                                                                           | Processes linger, so a later burst finds them warm                                                                            | Faster shrink to `min_procs`                                                                         | Next burst pays process start                                         |
| `DispatchTimeout`                      | default 30s                                                                           | Longer grace for a routed call to be accepted                                                                                 | Faster `ErrPluginUnavailable` on a stuck route                                                       | Nothing - it is a deadline                                            |
| `InvocationTimeout`                    | default 120s                                                                          | A slow call is allowed to finish                                                                                              | Faster failure; the slot and its permits free sooner                                                 | Held permits, until it fires                                          |
| `HealthInterval`                       | default 10s                                                                           | Fewer health RPCs competing with invocations                                                                                  | Faster detection of a wedged subprocess                                                              | -                                                                     |
| `DrainTimeout`                         | default 30s                                                                           | More time for in-flight calls on shutdown or promotion                                                                        | Faster promotion and shutdown, more calls cancelled                                                  | `CloseTimeout` is `DrainTimeout + 240s`                               |
| `CircuitCooldown`                      | default 5m                                                                            | A repeatedly failing deployment stays failed longer                                                                           | Faster retry after a transient host problem                                                          | All tracked invocations fail while the circuit is open                |
| `RetryMin` / `RetryMax` (manager)      | defaults 5s / 5m, 5 attempts                                                          | The manager waits longer before replacing a lost process                                                                      | Faster replacement, finite budget spent sooner                                                       | Queued calls wait; exhaustion opens the circuit                       |
| `ProcessOptions.RetryMin` / `RetryMax` | defaults 5s / 5m, 5 attempts                                                          | One process waits longer before restarting its subprocess                                                                     | Faster local restarts, budget spent sooner                                                           | `MessagePluginProcessRestartExhausted`, then the manager replaces it  |
| `MATCHER_TIMEOUT_SEC`                  | env                                                                                   | A slow matcher call is waited out                                                                                             | Faster whole-call failure, then retry                                                                | The service's own call context                                        |
| `MATCHER_MAX_ATTEMPTS`                 | env                                                                                   | More retries before dead-lettering                                                                                            | Faster dead-letters, shorter attempt tail                                                            | Batch stays uncommitted while retrying                                |
| `MATCHER_RETRY_BASE_MS` / `_CAP_MS`    | env                                                                                   | Longer waits between attempts                                                                                                 | Faster retries, more load on a failing plugin                                                        | The batch's own latency                                               |

The env knobs are read in [`cmd/event_matcher/matcher.go`](../../cmd/event_matcher/matcher.go), which owns their defaults.

## What moves when you change one value

Four inputs reach the runtime: `MAX_BATCH_SIZE`, `MAX_CONCURRENT_CALLS`, the container's CPU limit, and the artifact spec. Every budget above is derived from them in [`defaults.go`](../../internal/runtime/plugin/defaults.go).

The `101` is `MaxDeploymentProcs + 1`, not a batch size. It is the widest number of _concurrent_ invocations one call may make, so a batch of 10,000 events at `max_procs: 4` has 4 in flight. `MaxBatchSize` is a second, looser cap, since a chunk needs an item to carry; unset falls back to the process ceiling.

`MAX_BATCH_SIZE` is the only input that changes cost per event without changing the call count, and `max_procs` the only one that changes the fan-out without changing a budget.

## The admission chain

```text
Match                    -> MAX_CONCURRENT_CALLS semaphore      blocks
Submit  per-plugin share -> maxOutstandingInvocationsPerPlugin  REJECTS (ErrQueueFull)
Submit  shared budget    -> maxOutstandingInvocations           blocks
router                   -> no active route                     REJECTS (ErrPluginUnavailable)
manager pending queue    -> QueueSize                           REJECTS (ErrQueueFull)
manager dispatch         -> a ready process with a free slot     waits, then DispatchTimeout
plugin process           -> calls_per_process at a time          REJECTS (ErrQueueFull)
gRPC to the subprocess   -> 4 MiB receiver limit                 FAILS (ResourceExhausted)
plugin subprocess        -> the plugin's own answer              waits, then InvocationTimeout
```

Three layers reject rather than wait, all per-plugin: the admission share, the manager queue, and the plugin process, which refuses more calls than the capacity it published. The first two must each hold a whole fan-out per concurrent call.

Rejections are answers about capacity, so a retry can succeed. `ResourceExhausted` is an answer about shape: the retry fails identically and the batch dead-letters. The shared budget and the service semaphore only delay, so they are the right places to throttle.

## Tuning

- **Large events.** No knob here is the lever; fan-out and body width are. Give rules narrower `log_types` (an empty list matches every log type), and project the body down to the fields matchers read. `MAX_BATCH_SIZE` bounds events, not bytes: 45,000 events of 3 KB is 140 MB of raw JSON before decode, plus decoded maps and a cloned shard per chunk per matcher.
- **More throughput per event.** Set `MAX_BATCH_SIZE` explicitly, around 10,000 for small events; unset is 50. Cost per event falls up to that point as the projection reads and catalog clones amortise. Higher costs latency and memory: per-batch time rises linearly against `MATCHER_TIMEOUT_SEC`, the batch is held as messages plus decoded events plus one encoding per event, and the uncommitted replay window is one batch. Use ~1,000 when commit latency matters more.
- **Many plugins on few cores.** Lower `MAX_CONCURRENT_CALLS`: it bounds concurrent matcher calls and sizes the shared budget. Do not raise it above the default 8 - it multiplies against declared capacity, and every in-flight call holds its chunk's events.
- **A plugin whose calls are CPU-heavy.** Raise `max_procs`: its subprocesses run on separate cores and share nothing, and it pays only when cores are free. Leave `min_procs` at 0, since it bypasses the process budget - 50 matchers at `min_procs=4` is 200 subprocesses whatever the budget says.
- **A plugin whose calls wait on something else.** Raise `calls_per_process` above its default 32 rather than `max_procs`: a blocked call holds a subprocess without holding a core. It holds only where the plugin's own code is concurrency-safe, which the runtime cannot check; a plugin that is not safe sets `calls_per_process: 1`.
- **Bursty traffic.** Lower `ScaleCooldown` for a faster ramp, raise `IdleTimeout` to keep processes warm. `min_procs > 0` removes the first call's cold start.
- **Tail latency.** Lower `MATCHER_TIMEOUT_SEC`, `InvocationTimeout`, and `MATCHER_MAX_ATTEMPTS`. A batch commits only when every event is terminal, so the slowest call sets commit latency.

## Source references

- [`cmd/event_matcher/matcher.go`](../../cmd/event_matcher/matcher.go) - env knobs and defaults.
- [`pkg/matchers/application.go`](../../pkg/matchers/application.go) - grouping, chunking, worker budget: the fan-out.
- [`pkg/rules`](../../pkg/rules/application.go), [`pkg/enrichments`](../../pkg/enrichments/application.go), [`pkg/formatters`](../../pkg/formatters/application.go), [`pkg/tuning_rules`](../../pkg/tuning_rules/application.go) - the same path per item type.
- [`internal/runtime/rollout.go`](../../internal/runtime/rollout.go) - `RolloutBucketCount`, buckets, `RouteSides`.
- [`internal/runtime/shard.go`](../../internal/runtime/shard.go) - `MaxCallPayloadBytes`, `ChunkBounds`, `ShardBytes`.
- [`pkg/events/batch.go`](../../pkg/events/batch.go), [`pkg/alerts/batch.go`](../../pkg/alerts/batch.go) - shared encodings and prices.
- [`cmd/rule_executor/executor/executor.go`](../../cmd/rule_executor/executor/executor.go) - one encoding per message.
- [`internal/runtime/plugin/defaults.go`](../../internal/runtime/plugin/defaults.go) - derived budgets, timing defaults.
- [`internal/runtime/plugin/runtime_application.go`](../../internal/runtime/plugin/runtime_application.go) - `CallBudget`, admission, rejection paths.
- [`internal/runtime/plugin/deployment_manager.go`](../../internal/runtime/plugin/deployment_manager.go) - queueing, dispatch, scaling, idle shrink, circuit.
- [`internal/runtime/plugin/plugin_process.go`](../../internal/runtime/plugin/plugin_process.go) - one process, its subprocess session, `calls_per_process`.
- [`internal/runtime/plugin/process_budget.go`](../../internal/runtime/plugin/process_budget.go) - the process-wide budget.
