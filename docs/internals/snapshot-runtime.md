# Snapshot runtime

[Internals index](README.md) · [Controller runtime](controller-runtime.md) · [Plugin runtime](plugin-runtime.md) · [Runtime recovery](runtime-recovery.md)

The `internal/runtime/snapshot` subtree subscribes to one namespace's controller actor over the Ergo cluster and exposes a typed, immutable projection. Two construction sites: every plugin runtime builds one for its own namespace (external commit), and `event_matcher` builds a standalone rule subtree (direct commit).

## Composition

```mermaid
flowchart TB
  supervisor["1 snapshot.Supervisor[T]\nRestForOne, transient, intensity 5 / 10 s"]
  reader["1 reader actor\nfirst child"]
  controller["namespace controller actor\nremote"]
  projection["1 projection actor[T]\nsecond child"]
  supervisor --> reader
  supervisor --> projection
  reader <-->|Call/Send, cluster| controller
  supervisor -->|MessageExecutorReport| controller
```

- A reader restart restarts the projection behind it; a projection restart does not.
- No auto-shutdown. The supervisor keeps only the latest snapshot and reader-status events (buffer size 1).
- No meta process: Ergo pushes remote messages into the reader's mailbox.

## Messages

| Message                               | Direction                                                | Meaning                                                                       |
| ------------------------------------- | -------------------------------------------------------- | ----------------------------------------------------------------------------- |
| `MessageReaderActorActivate`          | snapshot supervisor → reader actor                       | Authorizes subscription and carries the `StatusEpoch` to continue after.      |
| `MessageProjectionActorActivate`      | snapshot supervisor → projection actor                   | Authorizes monitoring and carries the `StatusEpoch` to continue after.        |
| `MessageReaderActorStatusChanged`     | reader → supervisor → buffered status event              | Publishes epoch-tagged reader lifecycle, availability, committed generation.  |
| `MessageProjectionActorStatusChanged` | projection actor → snapshot supervisor                   | Publishes projection lifecycle, availability, committed/prepared generations. |
| `MessageProjectionCommit`             | external parent → snapshot supervisor → projection actor | Requests a PID/generation-fenced commit, external-commit mode only.           |
| `MessageProjectionCommitResult`       | projection actor → snapshot supervisor → external parent | Returns the fenced external-commit result.                                    |
| `MessageExecutorReportTick`           | snapshot supervisor → snapshot supervisor                | Periodic convergence-report timer.                                            |
| `MessageRadarTick`                    | snapshot supervisor → snapshot supervisor                | Periodic collector/readiness registration and heartbeat.                      |
| `MessageExecutorReport`               | snapshot supervisor → controller actor (cluster `Send`)  | Convergence report: generation received, generation held live.                |

## Roles

| Role                | Default name or identity                | Owner               | Responsibility                                                                      |
| ------------------- | --------------------------------------- | ------------------- | ----------------------------------------------------------------------------------- |
| Snapshot supervisor | `snapshot-<namespace>-supervisor`       | parent runtime      | Child PIDs, stable events, commit forwarding, convergence reporting.                |
| Reader actor        | `snapshot-<namespace>-reader-actor`     | snapshot supervisor | Controller subscription, committed generation, loss detection, resubscribe backoff. |
| Projection actor    | `snapshot-<namespace>-projection-actor` | snapshot supervisor | Typed parsed committed/prepared state.                                              |

- Names derive from `SupervisorOptions.Namespace` (the subtree's own, and the controller namespace its reader follows) via `SupervisorName`, `ReaderActorName`, `ProjectionActorName`, mirroring `controller-<namespace>-*`. None is configurable.
- `ReaderActorOptions` carries `Endpoint` and `ExecutorID`. The typed loader is a `NewSupervisor` parameter, not an option.
- Children get event identities at construction: the projection its two monitored names, the reader its one publication (name plus the `gen.Ref` token `SendEvent` requires).
- `ControllerActorName` names the subscription's far end.

## Readiness

Reader and projection status messages carry an `int64` Unix-nanosecond `StatusEpoch`, monotonically advanced even if the clock repeats or moves backward. The supervisor keeps one `statusEpoch` alongside each child's PID and status, accepting only newer epochs from the current PID and forwarding those timestamps unchanged. The epoch survives child replacement: the activation message carries it as `StatusEpoch`, and the replacement seeds its publisher's `lastStatusEpoch` from that floor. There are no separate incoming/outgoing epochs or duplicate supervisor-level status caches. Supervisor-generated lifecycle or projection identity changes advance the same epoch; replaying an unchanged status retains it.

`ProjectionCommitExternal` (matcher runtime) defers visibility to the parent; `ProjectionCommitDirect` (rule tree) makes a complete parsed snapshot visible at once. `Ready` needs a committed generation, a ready reader, and reader and observed generations at or beyond that commit.

## Snapshot supervisor

### Lifecycle

```mermaid
stateDiagram-v2
    [*] --> Starting
    Starting --> Running: both child PIDs recorded
    Starting --> Stopped: termination
    Running --> Stopped: termination or restart intensity exhausted
    Stopped --> [*]
```

Rest-for-one replacement changes child status and readiness, not the supervisor's `running` lifecycle.

### Messages

| Message                    | Direction                                     | Meaning                                                                                             |
| -------------------------- | --------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `HandleChildStart`         | Ergo supervisor runtime → snapshot supervisor | Records and activates a child incarnation.                                                          |
| `HandleChildTerminate`     | Ergo supervisor runtime → snapshot supervisor | Marks a child unavailable; reports external commit failure for a terminated projection.             |
| `gen.MessageDownProcessID` | Ergo process monitor → snapshot supervisor    | A restarted `radar_metrics` or `radar_health` lost its registration; the next tick re-registers it. |

### Readiness

The buffered event retains the latest reader status. External-commit mode adds the current projection PID and forwards status with its original timestamp, plus matching commit results. Radar readiness requires a running supervisor, both child PIDs, and ready reader/projection availability; child replacement can lower readiness without changing supervisor lifecycle.

### Executor reporting

The supervisor is the sole producer of `MessageExecutorReport{ExecutorID, Heartbeat, Applied, LastError}`, which the namespace controller folds into executor-convergence tracking (`/status`, `blink_controller_executors`, `blink_controller_executors_drifting`).

| Field                           | Source                                                                      | Meaning                                                                                                          |
| ------------------------------- | --------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| `Heartbeat.CommittedGeneration` | reader status `Generation`                                                  | The newest generation the controller has pushed here.                                                            |
| `Heartbeat.ReadyGeneration`     | projection status `CommittedGeneration`                                     | The generation this executor actually holds live.                                                                |
| `Heartbeat.Availability`        | projection availability, capped at `degraded` while the reader is not ready | A live projection with a dead reader still serves its last generation, but can no longer receive the next one.   |
| `Applied`                       | a projection commit that advanced the generation                            | Edge event; `Admitted` is false when the generation went live degraded.                                          |
| `LastError`                     | `runtime.FirstError` over supervisor, projection, then reader `LastError`   | The first non-nil error in the subtree, so a projection parse failure is reported even while the reader is fine. |

Under `ProjectionCommitExternal` the two diverge while the parent fetches binaries; only its commit makes the new generation live.

Reports are fire-and-forget, sent every `executorReportInterval` (30 s, a quarter of the controller's stale threshold) on a self-scheduled `MessageExecutorReportTick`, and immediately when a commit advances, reader availability or error text changes, projection error text changes, or the reader terminates.

## Reader actor

### Lifecycle

- On activation: one bounded `Call` (`SubscribeRequest{ExecutorID, KnownGeneration}`) to `ReaderActorOptions.Endpoint`, the controller actor at `gen.ProcessID{Name, Node}`, capped at 5 seconds.
- `SubscribeResponse` returns the committed snapshot (`Current`, nil before bootstrap) and `ControllerPID`; a `Current.Generation` newer than the last published is published at once.
- It `MonitorPID`s the controller and `MonitorNode`s its node, then publishes pushed `SnapshotUpdate`s newer than the last generation seen.
- `gen.MessageDownPID` (controller died) or `gen.MessageDownNode` (its node left) marks the reader unsubscribed and schedules a resubscribe carrying the last generation as `KnownGeneration`.
- The controller's `SubscribeRequest` handler does not currently read `KnownGeneration` and always returns the full committed snapshot. The reader skips republishing a burst that is not newer than `lastGeneration`.
- `Terminate` best-effort notifies the controller (`UnsubscribeRequest`).

```mermaid
stateDiagram-v2
    [*] --> Starting
    Starting --> Subscribing: parent activation
    Subscribing --> Ready: subscribe succeeds
    Ready --> Ready: newer SnapshotUpdate
    Ready --> Unsubscribed: controller PID or node down
    Subscribing --> Unsubscribed: subscribe fails
    Unsubscribed --> Subscribing: scheduled resubscribe
    Ready --> Restarting: supervisor records termination
    Unsubscribed --> Restarting: supervisor records termination
    Ready --> Stopped: terminate
    Unsubscribed --> Stopped: terminate
```

`Subscribing`, `Ready`, and `Unsubscribed` are all the `running` lifecycle with a different availability: controller loss keeps the actor alive and only resets `subscribed`. `restarting` is assigned by the supervisor when the child terminates, never by the reader itself.

### Messages

| Message                                     | Direction                                                 | Meaning                                                                                        |
| ------------------------------------------- | --------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `MessageReaderActorActivate`                | snapshot supervisor → reader actor                        | Authorizes the first subscribe and seeds `lastStatusEpoch` from the carried floor.             |
| `SubscribeRequest`/`Response`               | reader actor ↔ controller actor (cluster `Call`)          | Bounded handshake: registers the subscriber, returns the committed snapshot.                   |
| `SnapshotUpdate`                            | controller actor → reader actor (cluster `SendImportant`) | One commit's full state, pushed to every subscriber; applied only if newer than the last seen. |
| `UnsubscribeRequest`                        | reader actor → controller actor (cluster `Send`)          | Best-effort shutdown hint; controller-side `MonitorPID` is the authoritative removal path.     |
| `MessageSubscribeRetry`                     | reader actor → reader actor                               | Token-fenced resubscribe timer, sent at high priority.                                         |
| `gen.MessageDownPID`, `gen.MessageDownNode` | Ergo cluster monitor → reader actor                       | Marks the controller unreachable and schedules a resubscribe.                                  |
| `MessageReaderActorStatusChanged`           | reader actor → snapshot supervisor                        | Reports lifecycle, availability, last generation, and last error with a fresh epoch.           |
| `SendEvent`                                 | reader actor → snapshot supervisor event subscribers      | Publishes a committed snapshot to buffered and live consumers.                                 |

### Readiness

`Ready` only while subscribed; a controller loss or failed (re)subscribe reports `Unavailable`.

## Projection actor

### Lifecycle

It monitors buffered snapshot and reader-status events and parses every spec through its typed loader.

- A failed spec is skipped; the rest are prepared or committed, joined parse errors kept, and the actor reports degraded until a later generation parses cleanly.
- A generation with nothing parsed leaves the last commit intact and reports degraded.

`ProjectionClient` uses the stable child name and returns a deep clone.

```mermaid
stateDiagram-v2
    [*] --> Starting
    Starting --> Observing: activated, events monitored
    Observing --> Prepared: newer snapshot, >=1 parsed spec, external
    Observing --> Committed: newer snapshot, >=1 parsed spec, direct
    Prepared --> Committed: matching fenced commit
    Observing --> Degraded: skipped spec, or unparsable with prior commit
    Committed --> Ready: reader ready, reader and observed generations >= committed
    Ready --> Degraded: newer skipped or unparsable spec
    Ready --> Unavailable: reader not ready
    Committed --> Restarting: supervisor records termination
    Prepared --> Restarting: supervisor records termination
    Committed --> Stopped: terminate
    Prepared --> Stopped: terminate
```

`Observing` through `Ready` are all the `running` lifecycle with a different availability; only `restarting` and `stopped` are distinct lifecycles, and `restarting` is assigned by the supervisor on child termination.

### Messages

| Message                               | Direction                                              | Meaning                                                                              |
| ------------------------------------- | ------------------------------------------------------ | ------------------------------------------------------------------------------------ |
| `MessageProjectionActorActivate`      | snapshot supervisor → projection actor                 | Authorizes event monitoring and seeds `lastStatusEpoch` from the carried floor.      |
| `gen.MessageEvent`                    | snapshot supervisor event → projection actor           | Snapshot events drive observed, prepared, or committed state.                        |
| `gen.MessageEvent`                    | snapshot supervisor event → projection actor           | Reader-status events drive readiness.                                                |
| `MessageProjectionCommit`             | snapshot supervisor → projection actor (high priority) | Fenced external commit of one generation.                                            |
| `MessageProjectionCommitResult`       | projection actor → snapshot supervisor (high priority) | Commit outcome, stamped with the projection PID.                                     |
| `MessageProjectionActorStatusChanged` | projection actor → snapshot supervisor                 | Reports lifecycle, availability, committed and prepared generations, and last error. |
| `ProjectionStateRequest`              | `ProjectionClient.State` → projection actor            | Reads a deep-cloned current committed projection.                                    |
| `gen.MessageDownEvent`                | Ergo event monitor → projection actor                  | Terminates the projection when a monitored event ends.                               |

### Readiness

Direct mode commits a complete parsed generation on receipt. External mode commits when the requested generation matches observed and is prepared or already committed, otherwise `ErrProjectionNotPrepared`. Parents get stamped PIDs and commit results.

## Telemetry

Every layer publishes into the node's radar application, labelled by `namespace` from `SupervisorOptions.Namespace`, which `Init` requires. Plumbing: `internal/runtime/telemetry`, shared with the controller runtime.

| Layer            | Registers       | Publishes through | Notes                                                                                                        |
| ---------------- | --------------- | ----------------- | ------------------------------------------------------------------------------------------------------------ |
| Supervisor       | every collector | itself            | Registers through `gen.Node`. Publishes every gauge.                                                         |
| Reader actor     | -               | itself            | Subscribe results, accepted and dropped pushes, controller loss. Copies the supervisor's `telemetry.Labels`. |
| Projection actor | -               | itself            | Parse duration, result, per-spec failures, commit results.                                                   |

| Metric                                                                                                                                                                 | Published by     | Meaning                                                                                                       |
| ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------- | ------------------------------------------------------------------------------------------------------------- |
| `blink_snapshot_supervisor_lifecycle`                                                                                                                                  | supervisor       | 0 starting, 1 running, 2 stopped. Running once both children are up; no demotion on rest-for-one replacement. |
| `blink_snapshot_reader_availability`, `blink_snapshot_projection_availability`, `blink_snapshot_reported_availability`                                                 | supervisor       | 0 unavailable, 1 degraded, 2 ready; the third is what this executor reports.                                  |
| `blink_snapshot_reader_generation`, `blink_snapshot_projection_committed_generation`, `blink_snapshot_projection_prepared_generation`, `blink_snapshot_generation_lag` | supervisor       | Delivered, serving, awaiting-commit generations, and the local half of controller-side drift.                 |
| `blink_snapshot_commit_pending`, `blink_snapshot_executor_reports_total`                                                                                               | supervisor       | Generation whose external commit is in flight; convergence reports sent.                                      |
| `blink_snapshot_child_starts_total{child}`, `blink_snapshot_child_terminations_total{child,reason}`                                                                    | supervisor       | Reader and projection churn.                                                                                  |
| `blink_snapshot_subscribe_attempts_total{result}`, `blink_snapshot_controller_down_total{scope}`                                                                       | reader actor     | Subscribe outcomes; controller lost as process or whole node.                                                 |
| `blink_snapshot_updates_total`, `blink_snapshot_updates_ignored_total{reason}`                                                                                         | reader actor     | Pushed commits applied; drops as unsubscribed, wrong-sender, empty, or stale.                                 |
| `blink_snapshot_parses_total{result}`, `blink_snapshot_parse_failures_total`, `blink_snapshot_parse_seconds`                                                           | projection actor | A generation parses `ok`, `partial`, or `failed`. The failure counter is per spec.                            |
| `blink_snapshot_commits_total{result}`                                                                                                                                 | projection actor | External commit requests; `error` when not prepared.                                                          |

`MessageRadarTick` drives registration: sent from `Init`, retried every `telemetry.RadarTickInterval` (30 s). The supervisor owns the readiness-only `snapshot-<namespace>` signal: up only while running with live reader and projection children that both report ready. Projection-only status changes update readiness even when the reader status event is unchanged. Up signals are heartbeaten every tick and expire after 90 s; a stopped or unavailable subtree holds its signal down rather than unregistering it.

The supervisor monitors `radar_metrics` and `radar_health`, re-registering only what a restarted process lost. Health registration waits for its monitor to succeed. `MessageExecutorReport` remains the separate controller convergence protocol; Radar readiness does not replace it or affect liveness.

Emission is best-effort: an unreachable radar discards the `Send` error, a zero `telemetry.Labels` stays silent. Every `reconcileStatus()` refreshes gauges and readiness, even without a changed reader event; both the Radar tick and executor-report tick provide periodic refreshes. Both ticks use normal priority.

## Retry and shutdown

Supervisor lifecycle is `starting`, `running`, `stopped`; a child adds `restarting`, assigned by the supervisor. No draining stage; a stop is immediate.

- No actor accepts an unrelated synchronous call.
- Reader resubscribe uses `runtime.ScheduledBackoff`: multiplier 2, `DefaultRetryMin` 100 ms to `DefaultRetryMax` 5 s unless overridden, five retries, token invalidated on cancel or reset. The retry message is sent at high priority.
- Parent termination marks reader and projection stopped and unavailable.
- An optional `Stopped` channel receives the reason without blocking shutdown.

## Source references

- [`internal/runtime/snapshot/snapshot_supervisor.go`](../../internal/runtime/snapshot/snapshot_supervisor.go) - children, events, commit fencing.
- [`internal/runtime/snapshot/snapshot_reader_actor.go`](../../internal/runtime/snapshot/snapshot_reader_actor.go) - subscribe, controller loss, publication.
- [`internal/runtime/snapshot/subscription.go`](../../internal/runtime/snapshot/subscription.go) - `SubscribeRequest`/`Response`, `SnapshotUpdate`, `UnsubscribeRequest`, `MessageExecutorReport`, EDF.
- [`internal/runtime/snapshot/projection_actor.go`](../../internal/runtime/snapshot/projection_actor.go) - modes, parse, commit.
- [`internal/runtime/snapshot/metrics.go`](../../internal/runtime/snapshot/metrics.go) - metric specs.
- [`internal/runtime/telemetry/metrics.go`](../../internal/runtime/telemetry/metrics.go) - radar plumbing.
- [`internal/runtime/backoff.go`](../../internal/runtime/backoff.go), [`internal/runtime/snapshot/options.go`](../../internal/runtime/snapshot/options.go) - retry, options.
