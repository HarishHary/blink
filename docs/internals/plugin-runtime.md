# Plugin Runtime

`internal/runtime/plugin` owns local plugin deployment. One `plugin.Application[P,M]` bridges callers to one runtime supervisor on the process-owned Ergo node.

Only the runtime supervisor and the snapshot subtree have supervisor semantics. Snapshot, reconciler, and catalog are supervisor children. Routers, deployment routes, and managers are dynamic router mechanisms, not supervisors. Plugin processes are manager-spawned and manager-monitored. Metas are actor-spawned.

There is no actor pool between a manager and its processes. A plugin process advertises `calls_per_process`, so the manager schedules against advertised capacity rather than a worker count.

## Composition

```mermaid
flowchart TB
  app["plugin.Application"]
  supervisor["Runtime supervisor\nRestForOne, transient, intensity 5 / 5 s"]
  snapshot["1 snapshot.Supervisor[M]\nstatic supervisor child; external commit"]
  reconciler["1 reconciler actor\nstatic supervisor child"]
  resolver["1 artifact resolver meta\nactor-spawned"]
  watcher["1 artifact watcher meta\nactor-spawned"]
  catalog["1 catalog actor\nstatic supervisor child"]
  routers["N router actors\none per logical plugin; dynamic actors"]
  routes["N deployment routes per router\none per DeploymentRouteKey; dynamic routes, not supervisors"]
  managers["N deployment manager actors\none per deployment route; dynamic route processes, not supervisors"]
  procs["0..N plugin process actors per manager\nmin_procs..max_procs; manager-spawned and monitored, not supervised"]
  metas["N plugin metas / plugin subprocess sessions\none per plugin process; actor-spawned"]
  app --> supervisor
  supervisor --> snapshot
  supervisor --> reconciler
  supervisor --> catalog
  reconciler --> resolver
  reconciler --> watcher
  catalog --> routers --> routes --> managers --> procs --> metas
```

The runtime supervisor's RestForOne child order is snapshot, reconciler, catalog, so a prior child's restart restarts the later ones. It is transient, has intensity 5 in 5 seconds, handles child lifecycle itself, and does not auto-shutdown. The snapshot subtree's policy is in [snapshot-runtime.md](snapshot-runtime.md).

## Messages

| Message                     | Direction                                                                               | Meaning                                                                                        |
| --------------------------- | --------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| Admission and invocation    | caller → application → runtime supervisor → catalog → router → manager → plugin process | Carries an admitted production or shadow invocation to one plugin process.                     |
| Desired-state promotion     | reconciler → runtime supervisor → catalog → router                                      | Moves a resolved desired revision through the drain, freshness, and projection-commit barrier. |
| Status and readiness        | child/meta/plugin process → parent owner                                                | Aggregates fenced lifecycle, activity, and availability facts upward.                          |
| Drain                       | application → runtime supervisor → catalog → router → manager                           | Stops admission and drains dynamically owned work from the top down.                           |
| Completion and cancellation | plugin process → manager → router → catalog → runtime supervisor; application → runtime supervisor → catalog → router → manager | Completes one result idempotently or follows the fenced accepted/pending path to cancel it.    |

## Roles

| Role                   | Default name or identity                             | Owner                  | Responsibility                                                                        |
| ---------------------- | ---------------------------------------------------- | ---------------------- | ------------------------------------------------------------------------------------- |
| Plugin Application     | `<runtime-name>-application` application name        | Ergo application group | Caller lifecycle, production/shadow admission, and async-result ownership.            |
| Runtime Supervisor     | configured `<runtime-name>` (registered)             | Plugin Application     | Coordinates snapshot, reconciler, catalog, desired-state promotion, and drain.        |
| Snapshot Supervisor    | configured `<runtime-name>-snapshot` (registered)    | Runtime Supervisor     | Supplies reader/projection state with external projection commit.                     |
| Reconciler Actor       | `<runtime-name>-desired-state-reconciler` child name | Runtime Supervisor     | Resolves snapshot and local artifacts into desired router state.                      |
| Artifact Resolver Meta | dynamic `gen.Alias`                                  | Reconciler Actor       | Resolves locally valid artifacts and desired routes.                                  |
| Artifact Watcher Meta  | dynamic `gen.Alias`                                  | Reconciler Actor       | Reports artifact-directory watch/poll drift.                                          |
| Catalog Actor          | `<runtime-name>-catalog` child name                  | Runtime Supervisor     | Owns router incarnations and aggregates router status.                                |
| Router Actor           | dynamic PID/generation/epoch per plugin              | Catalog Actor          | Selects rollout routes and owns dynamic deployment routes.                            |
| Deployment Route       | stable SHA-256-derived atom per `DeploymentRouteKey` | Router Actor           | Creates, respawns, drains, and removes one manager route.                             |
| Deployment Manager     | dynamic route process PID                            | Deployment Route       | Owns bounded queueing, capacity-aware dispatch, scaling, process recovery, and drain. |
| Plugin Process         | dynamic linked-and-monitored actor PID               | Deployment Manager     | Serves up to `calls_per_process` invocations and recovers its plugin session.         |
| Plugin Meta            | dynamic `gen.Alias`                                  | Plugin Process         | Owns one plugin subprocess and RPC session.                                           |
| Invocation             | `callID` and one `runtime.Invocation` handle         | caller                 | Its `AsyncResult` remains owned by the application/runtime tree.                      |

The runtime and snapshot supervisors are registered. Every other name above is a child or dynamic identity.

## Readiness

The runtime admits work only when its expected projection generation is ready and committed, the desired-state barrier is idle, and the application is running.

The supervisor is ready when projection and catalog generations/revisions agree, catalog and projection are routable, and it is not draining. It gates on catalog routability rather than catalog readiness: one dead plugin must not withhold state from callers invoking the healthy ones, and per-call admission rejects the dead route with `ErrPluginUnavailable`.

Runtime availability is unavailable during a transition/drain or when projection/catalog is missing, and degraded when dependencies exist but are not all ready. Component readiness feeds this composite state; it adds no lifecycle constants.

## Plugin Application

### Lifecycle

The application is single-use: `New → Running → Stopping → Terminated`. `Start` marks it running only after registered-supervisor lookup succeeds. `Terminate` completes pending calls with `ErrRuntimeStopped`, or `ErrPluginUnavailable` when already stopping.

```mermaid
stateDiagram-v2
    [*] --> New
    New --> Running: Ergo application starts
    Running --> Stopping: Stop / drain request
    Running --> Terminated: supervisor exits
    Stopping --> Terminated: drain or stop completion
```

### Messages

| Message        | Direction                                         | Meaning                                                                                            |
| -------------- | ------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| `Start`        | Ergo application lifecycle → `plugin.Application` | Starts the single-use application after Ergo starts its group and finds its registered supervisor. |
| `Stop`         | Ergo application lifecycle → `plugin.Application` | Closes admission and begins runtime drain within Ergo's stop deadline.                             |
| `DrainRequest` | `plugin.Application` → runtime supervisor         | Requests bounded graceful drain.                                                                   |
| `Terminate`    | Ergo application lifecycle → `plugin.Application` | Completes pending calls with `ErrRuntimeStopped`, or `ErrPluginUnavailable` when already stopping. |

### Readiness

Admission takes two permits. `Submit` first reserves the plugin's own share, `MaxOutstandingInvocationsPerPlugin`, and rejects with `ErrQueueFull` when that share is full. Only then does it acquire the blocking application-wide `MaxOutstandingInvocations` permit, which includes queue waiters. `SubmitShadow` uses a separate non-blocking budget and returns `ErrShadowDropped` when full.

The per-plugin share stops one saturated plugin from consuming the shared budget and blocking every other plugin's caller until that caller's deadline expires. It fails its own calls fast instead.

Because the share rejects rather than waits, it is sized from the widest fan-out one caller batch can produce:

| Budget                                | Size                                                    | Why                                                                                                  |
| ------------------------------------- | ------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `MaxOutstandingInvocationsPerPlugin`  | `min(MaxBatchSize, MaxDeploymentProcs+1) * MaxConcurrentCalls` | One call is at most one shard per process a deployment may run, or the batch's own event count. |
| `MaxOutstandingInvocations`           | `perPlugin * MaxConcurrentCalls`                        | Nothing here knows how many plugins a batch touches; it only blocks, so it is a process-wide ceiling. |
| Shadow budget                         | a sixteenth of the shared one                           | Best-effort work must not be sized like production work.                                             |
| `QueueSize` (deployment manager)      | rises with the per-plugin share                         | One plugin's whole fan-out lands on one manager, and everything past its capacity waits there.       |

`MaxDeploymentProcs + 1` sits above both the process count and the two routing groups a batch can split into. A caller that declares no `MaxBatchSize` is sized for the widest fan-out one call is allowed. Callers ask for the smaller of the share and their deployment's declared capacity (`Application.CallBudget`), so a deployment declaring more than the share spends the rest over further calls.

A service that raises its batch size or concurrency must pass both, since a budget set apart from the fan-out it holds rejects legitimate calls.

These budgets bound concurrent calls, not the pieces a batch is cut into. A caller keeps each call's payload under the transport limit, and an oversized batch is cut into as many pieces as that takes and run through a bounded pool, so pieces can outnumber the capacity while live invocations never do. Calls are cheap to hold; the `ProcessBudget` under Deployment Manager bounds the subprocesses that serve them and is sized from CPUs instead. [concurrency-knobs.md](concurrency-knobs.md) tabulates all of these against every other knob that moves call counts.

## Runtime Supervisor

### Lifecycle

```mermaid
stateDiagram-v2
    [*] --> Starting
    Starting --> Running: catalog running
    Running --> Draining: DrainRequest
    Draining --> Stopped: catalog drained
    Running --> Restarting: child restart
    Restarting --> Running: children converged
```

### Messages

| Message                                        | Direction                                    | Meaning                                                                                   |
| ---------------------------------------------- | -------------------------------------------- | ----------------------------------------------------------------------------------------- |
| `MessageCatalogActivate`                       | runtime supervisor → catalog                 | Permits catalog status publication after child setup.                                     |
| `DrainRequest`                                 | application → runtime supervisor             | Starts the runtime's downward drain.                                                      |
| `MessageDrain`                                 | runtime supervisor → catalog                 | Drains the catalog after admission closes.                                                |
| `SupervisorStatusRequest`                      | `plugin.Application` → runtime supervisor    | Reads the availability/status snapshot.                                                   |
| `SupervisorStateRequest`                       | `plugin.Application` → runtime supervisor    | Reads the ready generation used for admission.                                            |
| `HandleChildStart`                             | Ergo supervisor runtime → runtime supervisor | Records each snapshot, reconciler, or catalog child incarnation and activates/replays it. |
| `HandleChildTerminate`                         | Ergo supervisor runtime → runtime supervisor | Retires a child incarnation and recomputes runtime availability.                          |
| `MessageReconcilerActorStatusChanged`          | reconciler → runtime supervisor              | Updates reconciler readiness and transition state.                                        |
| `MessageCatalogStatusChanged`                  | catalog → runtime supervisor                 | Updates catalog revision/readiness and completes transition checks.                       |
| `MessageCatalogDrained`                        | catalog → runtime supervisor                 | Completes drain waiters and terminates the runtime after calls are failed.                |
| `snapshot.MessageProjectionActorStatusChanged` | snapshot supervisor → runtime supervisor     | Updates committed/ready projection state for the external-commit subtree.                 |
| `MessageProjectionCommitRetry`                 | runtime supervisor → runtime supervisor      | Token-fenced deferred projection-commit retry.                                            |
| `MessageProjectionCommitDeadline`              | runtime supervisor → runtime supervisor      | Token-, PID-, and generation-fenced pending projection-commit expiry.                     |

### Readiness

The supervisor answers `SupervisorStatusRequest` and `SupervisorStateRequest`. Its exact state reader requires a nonzero ready generation equal to the committed generation, a routable committed projection, matching desired snapshot generation and catalog revision, a routable catalog, an idle transition, and a non-draining lifecycle.

After the catalog reports drained, the supervisor fails remaining calls, answers drain waiters, and terminates. The subtree uses independent finite retry domains, and cancelling a scheduled retry invalidates its token.

## Desired-State Transition

### Lifecycle

The reconciler proposes a monotonic desired revision once artifact resolution is complete and non-deferred. The supervisor holds a newer proposal until tracked invocations drain, applies it to the catalog, asks the reconciler to confirm freshness, then externally commits the matching projection generation.

```mermaid
stateDiagram-v2
    [*] --> Idle
    Idle --> Preparing: newer prepared desired state
    Preparing --> Preparing: in-flight calls drain
    Preparing --> AwaitingFreshness: catalog and reconciler converge
    AwaitingFreshness --> AwaitingProjection: exact freshness confirmation
    AwaitingProjection --> Idle: external projection commit acknowledged
    Preparing --> Draining: drain request
    AwaitingFreshness --> Draining: drain request
    AwaitingProjection --> Draining: drain request
```

### Messages

| Message                           | Direction                                | Meaning                                                                   |
| --------------------------------- | ---------------------------------------- | ------------------------------------------------------------------------- |
| `MessageProposeDesiredState`      | reconciler → runtime supervisor          | Offers a resolved, monotonic desired revision.                            |
| `MessageApplyCatalogDesiredState` | runtime supervisor → catalog             | Applies the revision after tracked calls drain.                           |
| `MessageDesiredStateFreshness`    | runtime supervisor → reconciler          | Challenges the proposed generation/revision after catalog convergence.    |
| `MessageDesiredStateFreshness`    | reconciler → runtime supervisor          | Confirms the exact generation/revision remains current and locally ready. |
| `MessageProjectionCommit`         | runtime supervisor → snapshot supervisor | Requests external commit of the matching projection generation.           |
| `MessageProjectionCommitResult`   | snapshot supervisor → runtime supervisor | Acknowledges the PID/generation-fenced projection commit.                 |

### Readiness

Admission reopens only after projection, reconciler, catalog, generation, and revision agree. This is transition/admission readiness, not an extra lifecycle enum.

The catalog side of the barrier is convergence, not aggregate health: every router must report the desired revision and be either ready or terminally failed (deployment circuit open). A router still starting holds the transition until it resolves either way. A plugin whose restart budget is spent never becomes healthy on its own, so gating on aggregate readiness would freeze every later generation.

## Reconciler Actor

### Lifecycle

The reconciler monitors the snapshot subtree's buffered snapshot/status events, starts resolver and watcher metas, and coalesces snapshot and filesystem changes.

```mermaid
stateDiagram-v2
    [*] --> Starting
    Starting --> Observing: activation and event monitors
    Observing --> Resolving: snapshot or artifact change
    Resolving --> Deferred: artifact not locally valid
    Deferred --> Resolving: scheduled retry or directory change
    Resolving --> Ready: resolved state proposed
    Ready --> Resolving: newer snapshot or directory change
    Observing --> Restarting: resolver or watcher meta down
    Restarting --> Observing: meta restart
    Ready --> Stopped: terminate
```

### Messages

| Message                               | Direction                                          | Meaning                                                                         |
| ------------------------------------- | -------------------------------------------------- | ------------------------------------------------------------------------------- |
| `MessageReconcilerActorActivate`      | runtime supervisor → reconciler                    | Activates event monitoring and sets the revision base.                          |
| `MessageResolveArtifacts`             | reconciler → artifact resolver meta                | Requests resolution for the current snapshot.                                   |
| `MessageArtifactResolutionResult`     | artifact resolver meta → reconciler                | Returns alias-, generation-, and dirtiness-fenced desired routes.               |
| `MessageArtifactDirectoryChanged`     | artifact watcher meta → reconciler                 | Marks artifact state dirty and triggers resolution.                             |
| `MessageResolutionRetry`              | reconciler → reconciler                            | Token-fenced retry timer for deferred or failed resolution.                     |
| `MessageArtifactResolverMetaRestart`  | reconciler → reconciler                            | Token-fenced timer that restarts the resolver meta.                             |
| `MessageArtifactWatcherMetaRestart`   | reconciler → reconciler                            | Token-fenced timer that restarts the watcher meta.                              |
| `gen.MessageEvent`                    | snapshot supervisor event → reconciler             | Buffered and live snapshot/reader-status events drive resolution and readiness. |
| `gen.MessageDownAlias`                | Ergo meta monitor → reconciler                     | Marks the resolver or watcher unavailable and schedules its restart.            |
| `MessageReconcilerActorStatusChanged` | reconciler → runtime supervisor                    | Publishes reconciler lifecycle, availability, generation, and revision.         |
| `SendExitMeta`                        | reconciler → Ergo meta runtime                     | Requests resolver/watcher meta termination during replacement or shutdown.      |
| `Terminate`                           | Ergo meta runtime → artifact resolver/watcher meta | Invokes meta cleanup after the parent's exit request.                           |

### Readiness

Resolver and watcher facts are fenced by meta alias. Resolution results are also fenced by snapshot generation and discarded when a newer filesystem or snapshot change made them dirty. Resolver, watcher, and resolution retries use separate shared scheduled-backoff instances, and retry timers carry tokens. Exhaustion is an actor failure, not an unbounded loop.

## Artifact Resolver Meta

### Lifecycle

The resolver has a one-slot job channel. It validates local names/specs, enabled state, and SHA-256 before producing desired primary/candidate routes.

```mermaid
stateDiagram-v2
    [*] --> Starting
    Starting --> Idle: meta spawned
    Idle --> Resolving: one resolve request
    Resolving --> Idle: alias-fenced result
    Idle --> Stopped: cancellation
    Resolving --> Failed: parent send failure
```

### Messages

| Message                           | Direction                                  | Meaning                                                        |
| --------------------------------- | ------------------------------------------ | -------------------------------------------------------------- |
| `MessageResolveArtifacts`         | reconciler → artifact resolver meta        | Supplies the snapshot to resolve.                              |
| `MessageArtifactResolutionResult` | artifact resolver meta → reconciler        | Returns resolved/deferred route candidates for its meta alias. |
| `SendExitMeta`                    | reconciler → Ergo meta runtime             | Requests termination of the resolver meta.                     |
| `Terminate`                       | Ergo meta runtime → artifact resolver meta | Invokes cleanup for the resolver's in-flight resolution.       |

### Readiness

Invalid or absent artifacts are deferred, not deployed. A successful alias-fenced result makes the current resolver meta ready for the reconciler's freshness check.

## Artifact Watcher Meta

### Lifecycle

```mermaid
stateDiagram-v2
    [*] --> Attaching
    Attaching --> Watching: directory readable and watch attached
    Attaching --> Polling: directory absent or watch attach failed
    Watching --> Debouncing: fsnotify event
    Debouncing --> Watching: drift published
    Watching --> Polling: watch invalidated or unreadable
    Polling --> Watching: poll recovery and attach
    Watching --> Stopped: cancellation
    Polling --> Stopped: cancellation
```

### Messages

| Message                              | Direction                                 | Meaning                                                 |
| ------------------------------------ | ----------------------------------------- | ------------------------------------------------------- |
| `MessageArtifactDirectoryChanged`    | artifact watcher meta → reconciler        | Reports a debounced filesystem or poll-detected change. |
| `MessageArtifactWatcherStateChanged` | artifact watcher meta → reconciler        | Reports watch/poll availability and drift state.        |
| `SendExitMeta`                       | reconciler → Ergo meta runtime            | Requests termination of the watcher meta.               |
| `Terminate`                          | Ergo meta runtime → artifact watcher meta | Invokes cleanup for fsnotify and polling.               |

### Readiness

The watcher combines fsnotify with a five-second metadata-fingerprint poll and a 300 ms notification debounce. A missing or unreadable directory is drift: it reports state and keeps polling rather than terminating. The resolver, not the watcher fingerprint, is the authority for content checksum.

## Catalog Actor

### Lifecycle

Catalog activation enables status publication. The catalog applies only nondecreasing desired revisions, dynamically creates one router incarnation per logical plugin ID, marks removed routers retiring, drains them, and removes their state.

```mermaid
stateDiagram-v2
    [*] --> Starting
    Starting --> Running: activated with desired revision
    Running --> Reconciling: desired state applied
    Reconciling --> Running: router statuses aggregate
    Running --> Restarting: desired router dies
    Restarting --> Running: scheduled router restart
    Running --> Draining: MessageDrain
    Draining --> Stopped: all routers drained
```

### Messages

| Message                           | Direction                    | Meaning                                                                |
| --------------------------------- | ---------------------------- | ---------------------------------------------------------------------- |
| `MessageCatalogActivate`          | runtime supervisor → catalog | Enables catalog status publication.                                    |
| `MessageApplyCatalogDesiredState` | runtime supervisor → catalog | Adds, updates, retires, or drains logical-plugin routers.              |
| `gen.MessageDownPID`              | Ergo monitor → catalog       | Reports a router incarnation death for PID/generation fencing.         |
| `MessageRouterRestart`            | catalog → catalog            | Token-fenced timer that retries creating a desired router.             |
| `MessageDrain`                    | runtime supervisor → catalog | Retires/drains every live router and suppresses restart.               |
| `MessageRouterStatusChanged`      | router → catalog             | Updates the PID/generation/epoch-fenced router aggregate.              |
| `MessageRouterDrained`            | router → catalog             | Retires a draining router and advances catalog drain/replacement work. |
| `MessageCatalogStatusChanged`     | catalog → runtime supervisor | Publishes the epoch-ordered aggregate catalog state.                   |
| `MessageCatalogDrained`           | catalog → runtime supervisor | Reports that every live router has drained.                            |

### Readiness

Router facts are accepted only from the current PID, generation, and increasing epoch. PID identifies the live process, generation identifies the catalog-created incarnation, and epoch orders that incarnation's status facts, so late messages cannot revive or overwrite a replacement. On router loss the catalog fails calls assigned to that PID and uses token-fenced `MessageRouterRestart` for a non-retiring desired router. Draining suppresses restart.

## Router Actor

### Lifecycle

Each router dynamically creates primary, canary, and shadow deployment routes from its desired state. A production call uses primary unless its rollout bucket selects an active canary. `SubmitShadow` fans out best-effort only to an active shadow candidate, so a full shadow budget never consumes production capacity.

The router routes a whole call by the single rollout key that call carries, so a batch only has to be cut where that decision changes:

- Callers gate submissions on `ProjectionData.RolloutByID[id].Shadow`, so a plugin with no shadow candidate never clones a batch for a call the router would discard.
- The same entry's `CanaryPct` gates the rollout split. `runtime.RouteSides` takes that percentage and the batch's rollout keys and answers the way the router will: no candidate, or one at 100% (elected primary), yields no groups and the batch is sent as it stands; a partial canary yields two, the buckets the candidate wins and the rest. It checks the percentage before walking the batch and the groups afterwards, so an undivided batch pays no per-item index or gather.
- A shadow candidate's own `rollout_pct` is not recorded there, since shadow calls are never routed by bucket.
- A candidate appearing mid-batch moves the committed generation, and calls from a retired generation are rejected so the caller re-resolves against fresh state.
- The entry also carries the shape a caller shards against. `MaxProcs` and `CallsPerProcess` are the larger of what the artifacts under that id declare, defaulted the way their deployments default them, and `Capacity()` is their product, so a batch fills the capacity whichever deployment it reaches can actually run.

```mermaid
stateDiagram-v2
    [*] --> Starting
    Starting --> Running: activated
    Running --> Reconciling: desired primary/candidate update
    Reconciling --> Running: routes status published
    Running --> Draining: MessageDrain
    Draining --> Stopped: all routes removed
```

### Messages

| Message                                 | Direction                   | Meaning                                                             |
| --------------------------------------- | --------------------------- | ------------------------------------------------------------------- |
| `MessageRouterActivate`                 | catalog → router            | Fences the new router incarnation with its Catalog generation.      |
| `MessageApplyRouterDesiredState`        | catalog → router            | Creates/updates primary, canary, and shadow route intent.           |
| `MessageInvokePlugin`                   | catalog → router            | Routes production or shadow work to an eligible deployment route.   |
| `MessageInvocationTimedOut`             | router → router             | Expires an unacknowledged route acceptance.                         |
| `MessageDrain`                          | catalog → router            | Drains and removes all deployment routes.                           |
| `MessageDeploymentManagerStatusChanged` | deployment manager → router | Updates a live route's manager status and active rollout selection. |
| `MessageDeploymentManagerDrained`       | deployment manager → router | Permits removal of a draining route.                                |
| `MessageDeploymentManagerTerminated`    | deployment manager → router | Fences manager loss and schedules an active-route recovery step.    |
| `MessageRouterStatusChanged`            | router → catalog            | Publishes router availability, rollout, and route state.            |
| `MessageRouterDrained`                  | router → catalog            | Reports that all routes were removed during drain.                  |

### Readiness

The router records an acceptance deadline before routing. A manager must acknowledge the call; unaccepted, route-failed, stale, or timed-out calls complete as unavailable. Cancellation goes to the accepted manager, or to the current route manager while acknowledgement is pending.

## Deployment Route

### Lifecycle

A route has a stable SHA-256-derived atom for one concrete `DeploymentRouteKey`. It is a dynamic router route, not a supervisor. Route status, acceptance, and termination are accepted only from the live manager PID; known past manager PIDs are retained only to fence late termination facts.

```mermaid
stateDiagram-v2
    [*] --> Pending
    Pending --> Active: AddRoute succeeds
    Pending --> Pending: retry AddRoute
    Pending --> Removed: obsolete desired route deleted
    Active --> Active: RespawnRoute after current manager loss
    Active --> Draining: deployment obsolete or router drain
    Draining --> Removing: manager drained
    Removing --> Removed: RemoveRoute succeeds
    Removing --> Removing: retry RemoveRoute
```

### Messages

| Message                                 | Direction                   | Meaning                                                                             |
| --------------------------------------- | --------------------------- | ----------------------------------------------------------------------------------- |
| `AddRoute`                              | router → `act.Router`       | Registers the stable route atom and starts its manager factory.                     |
| `MessageRetryRouteStep`                 | router → router             | Token-fenced retry for `AddRoute`, manager respawn, drain, or removal.              |
| `MessageDeploymentManagerStatusChanged` | deployment manager → router | Drives active-route readiness and router status.                                    |
| `MessageDeploymentManagerTerminated`    | manager → router            | Fences manager loss and schedules route recovery.                                   |
| `RespawnRoute`                          | router → `act.Router`       | Recreates the manager only after the active-route retry step, or to finish a drain. |
| `MessageDeploymentManagerDrained`       | manager → router            | Permits route removal after graceful manager drain.                                 |
| `RemoveRoute`                           | router → `act.Router`       | Removes the drained route; failures retry.                                          |

### Readiness

`AddRoute`, `RespawnRoute`, and `RemoveRoute` each retry through the route's independent `MessageRetryRouteStep` backoff. A pending route made obsolete is deleted directly. On active-manager loss the retry timer runs before `RespawnRoute`. Draining with no live manager respawns a draining manager so the normal drain protocol can complete.

## Deployment Manager

### Lifecycle

The manager validates `0 <= MinProcs <= max(1, MaxProcs) <= 100` and `MaxConcurrentCallsPerProcess <= MaxDeploymentCallsPerProcess` (64). It then schedules from what the deployment declared: `CapacityPerProcess()` is read at each use rather than cached, and returns the declared figure or the default `32`.

It acknowledges an invocation before queue admission so the router can bind its completion path, then rejects draining and open-circuit calls, and a full pending queue, with `ErrQueueFull`. Queue depth is bounded by `QueueSize`. The queue links the call entries themselves, so unlinking a cancelled or expired call costs the same wherever it sits - a plugin's queue is sized from its admission budget and runs thousands of entries deep, while callers cancel calls nowhere near the head.

The manager owns its plugin processes directly. Each is spawned with `LinkParent` and then `MonitorPID`: the link carries manager termination downward, the monitor reports a process death upward, and one dying subprocess never takes its deployment with it.

What the manager owns is a set of process slots. A slot is a stable identity that outlives the PIDs filling it, and holds what its current process reported, the invocations assigned to it, and the retry budget that owes it the next process.

| Field       | Holds                                                        |
| ----------- | ------------------------------------------------------------ |
| `processes` | slot state by monotonic slot id                              |
| `order`     | slot ids in the sequence they were opened                    |
| `byPID`     | a child's PID resolved back to the slot it fills             |

A process dying empties its slot instead of dropping it, which is what gives each slot its own retry budget. The deployment's processes are interchangeable and have no natural name to key that budget under, so a counted id stands in for one, as the router keys its children by plugin id. The ids are counted rather than positional because a restart message names the slot it is for and positions shift as slots are released; ids only grow, so the newest slot is the highest id.

Shrinking retires the newest slot holding nothing: a deployment that grew under load gives back what it just took, and never a process whose calls are still running. A process left above the desired count is retired by the reconciliation its next completion triggers.

Dispatch is capacity-aware rather than one-worker-per-call. Committed capacity is `ready processes x callsPerProcess`, and `selectProcess` hands the call to the ready process holding the fewest invocations. Least-loaded, not round-robin: one process serves several calls and finishes them at different times, so stacking the next call behind a busy one while a quiet process waits adds latency for nothing.

Scaling reasons in demand rather than in processes. Demand is active plus dispatching plus queued invocations, and `requiredProcs` is `ceil(demand / callsPerProcess)` clamped to `MinProcs..max(1, MaxProcs)`. One process carries `callsPerProcess` of that demand, so the count a queue needs falls as declared capacity rises.

| Direction | Conditions                                                                                              | Step                                          |
| --------- | ------------------------------------------------------------------------------------------------------- | --------------------------------------------- |
| Growth    | every ready process at capacity, a call still waiting, every desired process ready, 1 s scale cooldown   | straight to the required count in one pass    |
| Shrink    | required below desired, queue empty, nothing dispatching, 30 s idle, a process holding no invocation     | one process per cooldown, arming the next pass |

Growth goes to the required count at once so a wide `calls_per_process` does not ramp a second at a time. Shrink arms its own next pass because nothing else reconciles a deployment that has gone quiet, and it gives back only an empty process - demand counts active calls, so the deployment stays at the count those calls need. At a zero minimum the last process goes too and the deployment sleeps holding nothing.

`MaxProcs` is only this deployment's own ceiling. Because every plugin process is a subprocess, growth past a deployment's reserved `max(1, MinProcs)` also needs a permit from one `ProcessBudget` shared by every manager in the process, sized `GOMAXPROCS x DefaultRuntimeProcessGrowthPerProc` (2) so the count follows the container's CPU limit rather than the node's core count.

- Reservations are outside the budget. Desired state always gets its `MinProcs`, and a `MinProcs=0` route always gets the one process a queued call wakes, since a route that could start no process would fail every call routed to it.
- Reservations are counted only to warn: a catalog whose reservations exceed the budget logs `reserved plugin processes exceed the process budget` and starts anyway.
- A denied permit is not an error. The calls stay queued and the cooldown paces the next attempt.
- Permits are returned when the deployment shrinks, when its circuit opens, and when the manager terminates, so a process at its budget still moves growth to whichever deployment has queued work.

```mermaid
stateDiagram-v2
    [*] --> Starting
    Starting --> Running: ready capacity or zero-process idle
    Running --> Running: only some processes ready => availability is degraded
    Running --> Starting: the last ready process is lost
    Starting --> Running: a replacement process reports ready
    Starting --> Failed: a slot's restart budget exhausted => circuit opens
    Failed --> Starting: circuit cooldown opens fresh slots
    Running --> Draining: MessageDrain
    Draining --> Stopped: calls and processes drained
    Draining --> Stopped: drain deadline expires
```

### Messages

| Message                                    | Direction                                                                         | Meaning                                                                                                                        |
| ------------------------------------------ | --------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `MessageInvokePlugin`                      | runtime supervisor → catalog → router → deployment manager                        | Acknowledges ownership, then queues or rejects the routed call.                                                                |
| `MessageInvocationAccepted`                | deployment manager → router                                                       | Binds the router's completion/cancellation path to this manager PID.                                                           |
| `MessageCancelInvocation`                  | `plugin.Application` → runtime supervisor → catalog → router → deployment manager | Cancels the accepted or pending invocation at its fenced manager.                                                              |
| `MessageDeploymentManagerDispatchDeadline` | manager → manager                                                                 | Times out an invocation awaiting process acceptance.                                                                           |
| `MessageDeploymentManagerRestart`          | manager → manager                                                                 | Token-fenced retry that refills one named slot.                                                                                |
| `MessageDeploymentManagerReconcile`        | deployment manager → deployment manager                                           | Token-fenced autoscaling pass.                                                                                                 |
| `MessageDeploymentManagerCircuitCooldown`  | manager → manager                                                                 | Token-fenced circuit re-arm: opens a fresh set of slots after the cooldown.                                                    |
| `MessageDeploymentManagerDrainDeadline`    | manager → manager                                                                 | Cancels remaining work with `context.DeadlineExceeded`.                                                                        |
| `MessageDrain`                             | router → deployment manager                                                       | Stops new work, cancels recovery/scale activity, and starts graceful drain.                                                    |
| `MessageInvokePlugin`                      | deployment manager → plugin process                                               | Dispatches one queued call to the least-loaded ready process.                                                                  |
| `MessageStop`                              | deployment manager → plugin process                                               | Asks a retired process to finish once its last invocation completes.                                                           |
| `MessagePluginProcessStatusChanged`        | plugin process → deployment manager                                               | Drives capacity, availability, dispatch, and scaling; a process reaching ready resets its own slot's budget.                  |
| `MessageInvocationStarted`                 | plugin process → deployment manager                                               | Clears the dispatch deadline and marks the call active.                                                                        |
| `MessageInvocationFinished`                | plugin process → deployment manager                                               | Returns the invocation result and frees that much of the process's capacity.                                                   |
| `MessagePluginProcessRestartExhausted`     | plugin process → deployment manager                                               | Retires only the spent process and owes it a backoff-paced replacement.                                                        |
| `MessagePluginProcessStopped`              | plugin process → deployment manager                                               | Fails calls dispatched to a stopping process before its DOWN arrives.                                                          |
| `gen.MessageDownPID`                       | Ergo monitor → deployment manager                                                 | Empties the slot the lost process filled and starts that slot's own bounded replacement when unexpected.                       |
| `MessageDeploymentManagerStatusChanged`    | deployment manager → router                                                       | Publishes process, lifecycle, and availability changes; queue counters ride along, and an unchanged status is not republished. |
| `MessageDeploymentManagerDrained`          | deployment manager → router                                                       | Reports that no invocation or process remains.                                                                                 |
| `MessageRetryDeployment`                   | no production sender                                                              | Defined router control message; no production path emits it.                                                                   |
| `MessageDeploymentManagerRetry`            | router → deployment manager                                                       | Authenticated circuit reset if a caller sends the otherwise unproduced retry message.                                          |

### Readiness

`MinProcs=0` starts no process; the first queued call wakes one. Dispatch requires available capacity beyond active plus dispatching calls and is bounded by `MessageDeploymentManagerDispatchDeadline`. Each invocation is bounded by the process's own invocation timeout, and graceful drain by `MessageDeploymentManagerDrainDeadline`.

Two recovery budgets are independent:

| Budget                              | Paces                                          | Owner  |
| ----------------------------------- | ---------------------------------------------- | ------ |
| `ProcessOptions.RetryMin/RetryMax`  | one process restarting its own subprocess      | process |
| manager `RetryMin/RetryMax`         | refilling a slot whose process the manager lost | slot   |

Each slot owns one manager budget, so a deployment that loses a different process now and then never spends a single shared one, and a slot waiting on its backoff does not hold back the others. A process reporting ready resets its own slot's budget, so a deployment that loses one process a day does not eventually open its circuit for a fault it recovers from every time.

A process reporting restart exhaustion is retired alone, and its replacement is owed rather than started: the slot counts toward `runningProcs` until its DOWN arrives, then waits on its own backoff, so that backoff decides when the successor starts. The remaining processes keep serving the calls they hold, so a partially broken deployment reports `running` with degraded availability.

Exhausting any one slot's budget opens the deployment's circuit. That fails every tracked invocation, drops the desired count back to `MinProcs`, returns every grown permit to the process budget, releases every slot, and stops recovery. The circuit re-arms itself: `openCircuit` schedules a token-fenced `MessageDeploymentManagerCircuitCooldown` after `CircuitCooldown` (default 5 minutes), and handling it reconciles, which opens fresh slots with fresh budgets. A deployment broken by a transient host problem recovers without an operator; one genuinely broken re-opens the circuit. Drain and terminate cancel the pending cooldown.

`MessageDeploymentManagerRetry` resets the circuit immediately, but no production sender emits `MessageRetryDeployment`. An active route is otherwise unchanged by the circuit; it changes only when desired state removes or replaces it. `Recovering` is composite-state prose, not a `DeploymentManagerLifecycle` constant - those are `starting`, `running`, `draining`, `failed`, and `stopped`.

A deployment is ready when its ready process count covers a nonzero `MinProcs`, degraded when only some processes are ready or while it drains, and unavailable while it starts with none ready or while its circuit is open. A `MinProcs=0` deployment holding no process, no call, no pending retry, and no error is ready: it is asleep, not broken.

Status is published only when health changes - lifecycle, availability, desired or ready counts, per-process capacity, total capacity, last error, or any owned process's status. Queue depth, dispatching, active, and available capacity ride along with the next health change, because every accepted, dispatched, and completed invocation reconciles this manager and the router recomputes its own status for each fact it receives.

## Plugin Process

### Lifecycle

Each plugin process owns one replaceable meta alias, uses independent normal and health restart budgets, and has `idle`/`busy`/`saturated` activity separate from lifecycle. A ready process periodically pings the meta; a ping timeout or error retires its alias and takes the health-recovery path. Exhaustion marks the process failed and notifies its manager, which replaces that one process rather than the deployment.

```mermaid
stateDiagram-v2
    [*] --> Starting
    Starting --> Ready: plugin meta started
    Ready --> Busy: an invocation accepted below calls_per_process
    Ready --> Saturated: an invocation accepted that fills calls_per_process
    Busy --> Saturated: the last free slot is taken
    Saturated --> Busy: a slot frees with calls still in flight
    Busy --> Ready: last invocation answered
    Saturated --> Ready: last invocation answered at a capacity of one
    Ready --> Restarting: meta down or health failure
    Busy --> Restarting: transport failure or timeout
    Saturated --> Restarting: transport failure or timeout
    Restarting --> Ready: meta restart
    Restarting --> Failed: restart budget exhausted
```

### Messages

| Message                                | Direction                           | Meaning                                                                 |
| -------------------------------------- | ----------------------------------- | ----------------------------------------------------------------------- |
| `MessageInvokePlugin`                  | deployment manager → plugin process | Delivers one dispatched call to the process that owns the subprocess.   |
| `MessageInvocationStarted`             | plugin process → deployment manager | Confirms acceptance and clears the dispatch deadline.                   |
| `MessageInvocationFinished`            | plugin process → deployment manager | Returns the one-shot invocation result.                                 |
| `MessagePluginMetaRestart`             | plugin process → plugin process     | Token-fenced normal or health restart timer.                            |
| `MessagePluginMetaHealthTick`          | plugin process → plugin process     | Drives the next meta Ping attempt.                                      |
| `MessagePluginMetaHealthTimeout`       | plugin process → plugin process     | Retires an unanswered meta health check.                                |
| `MessagePluginMetaInvokeTimeout`       | plugin process → plugin process     | Per-call backstop for an invocation the meta never answered.            |
| `MessagePluginMetaStartResult`         | plugin meta → plugin process        | Drives ready state or normal restart.                                   |
| `MessagePluginMetaPing`                | plugin process → plugin meta        | Performs the parent-authorized health RPC.                              |
| `MessagePluginMetaPingResult`          | plugin meta → plugin process        | Drives health confirmation or health restart.                           |
| `gen.MessageDownAlias`                 | Ergo meta monitor → plugin process  | Retires the current meta and starts normal/health recovery.             |
| `MessagePluginProcessStatusChanged`    | plugin process → deployment manager | Publishes lifecycle, availability, and idle/busy/saturated activity.    |
| `MessagePluginProcessStopped`          | plugin process → deployment manager | Reports process shutdown so its calls fail at once.                     |
| `MessagePluginProcessRestartExhausted` | plugin process → deployment manager | Escalates exhausted local recovery for this process alone.              |
| `MessageStop`                          | deployment manager → plugin process | Stops normally after the manager retired it and its last call finished. |
| `SendExitMeta`                         | plugin process → Ergo meta runtime  | Requests plugin-meta termination before recovery.                       |
| `Terminate`                            | Ergo meta runtime → plugin meta     | Invokes meta cleanup after the process's exit request.                  |

### Readiness

The process is ready only when its current meta alias is ready. Lifecycle, availability, and idle/busy/saturated activity are distinct parts of its composite state.

Dispatch does not block this actor. `invoke` reports `MessageInvocationStarted`, wraps the caller's context in the process's own `InvocationTimeout`, records the call in `p.calls`, and sends a `pluginMetaInvoke` message to the meta alias. The answer arrives later as `pluginMetaInvokeResult` and completes the call. Nothing waits in between, so health checks, further invocations, restart timers, and manager messages are all served while a plugin works. Each entry therefore records the meta incarnation - a monotone `generation` bumped whenever a new alias is adopted - so an answer from a replaced subprocess finds no live entry and is dropped.

Three things end a call:

- **Its answer.** If the answer is `recycle`, the process reports itself unavailable to its manager before completing the call; the call reports its own failure and only its siblings inherit the generic `ErrProcessRecycle`. Order matters: the manager routes from its own copy of this process's availability, which is one message behind, and completing the call is what frees the capacity that makes the manager dispatch again. A completion sent first provokes a dispatch decided on lapsed readiness, and with one process there is nowhere else for that call to go. Retiring the meta reports the same unavailability again, so `reportUnavailable` is idempotent.
- **A retired or DOWN meta.** Those calls can never be answered, so `failGenerationCalls` fails them with `ErrProcessRecycle` rather than leaving callers to time out.
- **`MessagePluginMetaInvokeTimeout`,** armed for the caller's remaining deadline plus `pluginMetaCancelGrace` plus a second of slack. The meta answers a cancelled or expired call inside its grace, so this timer firing means a hung subprocess. It carries only the call id, which is fence enough: a completed call is no longer tracked and ids are never reused.

Shutdown cancels every in-flight call's context. The manager fails those calls when it sees the process stop, but the subprocess should stop working on them regardless.

Invocation capacity is the deployment's `calls_per_process`, `32` unless the artifact declared its own. Nothing in this layer serializes invocations: `p.calls` holds every call in flight and each is a message to the meta alias, so a declared capacity of four really runs four. Capacity cannot make the plugin re-entrant - the subprocess holds one plugin object and gRPC hands every RPC its own goroutine - so the default asks every plugin to be concurrency-safe, and a plugin that is not declares `calls_per_process: 1`. A call arriving beyond capacity is refused with `ErrQueueFull` rather than queued here, since exceeding the published capacity means the manager's view is stale. That check is the last one `invoke` makes, after a dead context, a missing callback, unreadiness, and a duplicate call ID.

Activity follows the same number. `refreshActivity` labels the process `saturated` once `len(p.calls)` reaches capacity, `busy` below that, and `idle` at none, and samples `inFlight` and `capacity` on every change. At a declared capacity of one, saturated is the label a working process publishes and busy never appears. Only a label change publishes a status - the two counters ride along - so a deployment pays no status publish per invocation.

## Plugin Meta

### Lifecycle

```mermaid
stateDiagram-v2
    [*] --> Launching
    Launching --> Serving: checksum, gRPC connect, dispense, Init handshake
    Serving --> Pinging: parent health request
    Pinging --> Serving: Ping succeeds
    Pinging --> Closing: Ping fails
    Serving --> Invoking: parent invocation message
    Invoking --> Serving: callback answered
    Invoking --> Closing: recycle-worthy transport/context failure
    Closing --> Stopped: Shutdown RPC then subprocess kill
```

### Messages

| Message                        | Direction                          | Meaning                                                               |
| ------------------------------ | ---------------------------------- | --------------------------------------------------------------------- |
| `MessagePluginMetaStartResult` | plugin meta → plugin process       | Reports checksum/handshake/session startup outcome for its alias.     |
| `MessagePluginMetaPing`        | plugin process → plugin meta       | Requests a parent-authorized health RPC.                              |
| `MessagePluginMetaPingResult`  | plugin meta → plugin process       | Returns the alias-fenced Ping result.                                 |
| `pluginMetaInvoke`             | plugin process → plugin meta       | Parent-only callback invocation, run without blocking either side.    |
| `pluginMetaInvokeResult`       | plugin meta → plugin process       | Returns one call's result and whether the session must be recycled.   |
| `Shutdown`                     | plugin meta → plugin RPC           | Bounded session shutdown before client kill.                          |
| `SendExitMeta`                 | plugin process → Ergo meta runtime | Requests meta termination after a recycle-worthy failure or shutdown. |
| `Terminate`                    | Ergo meta runtime → plugin meta    | Invokes session close after the process's exit request.               |

### Readiness

The meta verifies the artifact checksum before `exec.CommandContext`, requires the configured gRPC handshake, and runs `Init`. Only its parent plugin process may call or ping it. On close it sends `Shutdown` with a three-second bound then kills the client. Plugin `Unavailable`, malformed responses, transport failures, and health failures retire the alias and enter bounded health recovery.

Cancellation and deadlines are classified rather than assumed fatal, because killing the subprocess is this layer's only isolation mechanism and a shared subprocess would make every sibling call pay for one caller's withdrawal. `classifyInvocation` recycles on `Unavailable`, and on `Canceled` or `DeadlineExceeded` only when the caller's own context is still live - that is the transport reporting its own failure rather than a caller withdrawing a request.

When the caller's context ends while the callback is still running, the meta waits out `pluginMetaCancelGrace` (one second) before deciding. A plugin that honours its RPC context returns inside that window and keeps its subprocess; one that ignores cancellation can be stopped no other way, since Go cannot kill the goroutine running an arbitrary callback. The context the plugin sees carries the caller's deadline but not the grace: no context can express "one second after this other context ends", and none can stop that goroutine. The grace only decides when to stop waiting. Its owner arms an independent backstop timer past the grace so its own giving-up never races this decision, and substitutes the caller's reason only when the call returned nothing.

An answer is sent to the owner before a fatal one closes the session. Closing first would race that message against the meta's own DOWN, and the caller would be told its call was recycled rather than why.

## Invocation

### Lifecycle

```mermaid
stateDiagram-v2
    [*] --> Submitted
    Submitted --> Admitted: plugin share plus production permit, or shadow TryAcquire
    Submitted --> Rejected: lifecycle, generation, plugin share full, or admission failure
    Admitted --> Routed: supervisor, catalog, router
    Routed --> Accepted: manager acknowledgement
    Routed --> Failed: route acceptance deadline or route failure
    Accepted --> Dispatching: manager to plugin process
    Dispatching --> Running: process accepted the call
    Running --> Completed: plugin result
    Submitted --> Cancelling: caller context or Cancel
    Routed --> Cancelling: cancellation forwarded
    Accepted --> Cancelling: cancellation forwarded
    Dispatching --> Cancelling: cancellation forwarded
    Running --> Cancelling: cancellation forwarded
    Cancelling --> Completed: normal terminal completion path
    Completed --> [*]
    Failed --> [*]
    Rejected --> [*]
```

### Messages

| Message                      | Direction                                                                         | Meaning                                                              |
| ---------------------------- | --------------------------------------------------------------------------------- | -------------------------------------------------------------------- |
| `Submit`                     | caller → `plugin.Application`                                                     | Acquires the blocking production permit.                             |
| `SubmitShadow`               | caller → `plugin.Application`                                                     | Uses the separate non-blocking best-effort shadow permit.            |
| `MessageSubmitInvocation`    | `plugin.Application` → runtime supervisor                                         | Enters an admitted invocation into the runtime tree.                 |
| `MessageInvokePlugin`        | runtime supervisor → catalog → router → deployment manager → plugin process       | Carries the routed call to one plugin process.                       |
| `MessageCancelInvocation`    | `plugin.Application` → runtime supervisor → catalog → router → deployment manager | Follows the fenced accepted/pending manager path.                    |
| `MessageInvocationAccepted`  | deployment manager → router                                                       | Binds the routed call to its manager before completion/cancellation. |
| `MessageInvocationStarted`   | plugin process → deployment manager                                               | Moves the accepted call from dispatching to running.                 |
| `MessageInvocationFinished`  | plugin process → deployment manager                                               | Returns the plugin invocation result.                                |
| `MessageInvocationCompleted` | manager → router → catalog → runtime supervisor                                   | Completes the idempotent `runtime.AsyncResult` once.                 |

### Readiness

Submission requires a running application and a runtime whose expected generation is exactly ready and committed; draining or a desired-state barrier rejects it as unavailable. Every terminal path enters the idempotent `runtime.AsyncResult`, and `Invocation` closes `Done` only when that result completes.

Call IDs bind one supervisor, catalog, router, manager, and plugin-process path. PID/alias plus generation/epoch checks reject stale completion, status, and recovery facts, and the manager accepts an invocation fact only from the process it dispatched that call to.

## References

- [`internal/runtime/plugin/runtime_application.go`](../../internal/runtime/plugin/runtime_application.go) - application lifecycle, admission, State, submit, completion.
- [`internal/runtime/plugin/runtime_supervisor.go`](../../internal/runtime/plugin/runtime_supervisor.go) - RestForOne tree, desired-state barrier, projection commit, drain.
- [`internal/runtime/plugin/reconciler_actor.go`](../../internal/runtime/plugin/reconciler_actor.go), [`artifact_resolver_meta.go`](../../internal/runtime/plugin/artifact_resolver_meta.go), and [`artifact_watcher_meta.go`](../../internal/runtime/plugin/artifact_watcher_meta.go) - desired state and local artifact facts.
- [`internal/runtime/plugin/catalog_actor.go`](../../internal/runtime/plugin/catalog_actor.go) and [`router_actor.go`](../../internal/runtime/plugin/router_actor.go) - router ownership, route lifecycle, rollout, and fences.
- [`internal/runtime/plugin/deployment_manager.go`](../../internal/runtime/plugin/deployment_manager.go), [`process_budget.go`](../../internal/runtime/plugin/process_budget.go), [`plugin_process.go`](../../internal/runtime/plugin/plugin_process.go), and [`plugin_process_meta.go`](../../internal/runtime/plugin/plugin_process_meta.go) - queue, capacity-aware dispatch, plugin processes, subprocess, recovery, and drain.
- [`internal/runtime/invocation.go`](../../internal/runtime/invocation.go) and [`internal/runtime/backoff.go`](../../internal/runtime/backoff.go) - one-shot completion and shared scheduled backoff.
