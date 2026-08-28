# Controller service

[Services index](README.md) · [Runtime design](../internals/controller-runtime.md)

`cmd/controller` is Blink's control-plane binary. It starts five independent Ergo controller applications, each watching one local artifact catalog, persisting its state to SQLite, and pushing that catalog's effective entries directly to every subscribed executor over the native Ergo cluster. It does not process pipeline events or start plugin binaries, and there is no broker in its distribution path.

## Process composition

```mermaid
flowchart TB
  main[cmd/controller]
  health[health service :8080]
  main --> rule[controller-rule\nnamespace: rule]
  main --> matcher[controller-matcher\nnamespace: matcher]
  main --> tuning[controller-tuning\nnamespace: tuning]
  main --> formatter[controller-formatter\nnamespace: formatter]
  main --> enrichment[controller-enrichment\nnamespace: enrichment]
  main --> health
  rule -->|SnapshotUpdate, cluster| ruleExecutors[rule_executor pods]
  matcher -->|SnapshotUpdate, cluster| matcherExecutors[event_matcher pods]
  tuning -->|SnapshotUpdate, cluster| tuningExecutors[rule_tuner pods]
  formatter -->|SnapshotUpdate, cluster| formatterExecutors[alert_formatter pods]
  enrichment -->|SnapshotUpdate, cluster| enrichmentExecutors[alert_enricher pods]
```

| Message                                             | Direction                                                           | Meaning                                                                                 |
| --------------------------------------------------- | ------------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| `plugin.Start`                                      | `cmd/controller` → Ergo node                                        | Starts the node, cluster networking enabled, named `controller@<CONTROLLER_NODE_HOST>`. |
| `services.Runner.Register`, `controller.NewService` | `cmd/controller` → controller services                              | Registers the five namespace-specific controller services.                              |
| `services.NewHealthService`                         | `cmd/controller` → health service                                   | Registers the HTTP health service on `:8080`.                                           |
| `Application.Load`                                  | controller service → namespace application                          | Creates each application, opens its SQLite handle, and registers its EDF network types. |
| `SubscribeRequest`/`SnapshotUpdate`                 | subscribing executor ↔ namespace controller actor (cluster)         | Registers a subscriber and pushes each completed generation to it.                      |
| `MessageExecutorReport`                             | executor snapshot supervisor → namespace controller actor (cluster) | Reports that executor's received and live generations, every 30 s and on change.        |

| Service name            | Namespace    | Catalog directory      | Loader                |
| ----------------------- | ------------ | ---------------------- | --------------------- |
| `controller-rule`       | `rule`       | `RULE_PLUGIN_DIR`      | `rules.Loader`        |
| `controller-matcher`    | `matcher`    | `MATCHER_PLUGIN_DIR`   | `matchers.Loader`     |
| `controller-tuning`     | `tuning`     | `TUNER_PLUGIN_DIR`     | `tuning_rules.Loader` |
| `controller-formatter`  | `formatter`  | `FORMATTER_PLUGIN_DIR` | `formatters.Loader`   |
| `controller-enrichment` | `enrichment` | `ENRICHER_PLUGIN_DIR`  | `enrichments.Loader`  |

All five use `CONTROLLER_DATABASE_DSN`. The database tables are scoped by the namespace, so a shared DSN retains separate catalog records, generations, and snapshots. `ETCD_ENDPOINTS`, `CLUSTER_COOKIE`, and `CONTROLLER_NODE_HOST` configure the cluster the process joins - every executor subscribing to this controller must share the same cookie and be able to resolve `controller@<CONTROLLER_NODE_HOST>` through the same etcd registrar.

## Operational lifecycle

The process creates an Ergo node named `controller@<CONTROLLER_NODE_HOST>` (its stable, cluster-reachable Service DNS name - not `localhost`, since executors on other pods must reach it), starts the five registered controller services and the health service concurrently, and waits for `SIGINT` or `SIGTERM`. Each controller service constructs a fresh application for every runner attempt.

```mermaid
stateDiagram-v2
  [*] --> NewApplication
  NewApplication --> Load: ApplicationLoad
  Load --> Start: ApplicationStart
  Load --> Cleanup: load/start error
  Start --> GracefulStop: context cancelled
  Start --> Cleanup: application terminated
  GracefulStop --> Cleanup: stopped or 45s timeout
  Cleanup --> Unload: sealed, writer I/O quiesced, resources closed
  Unload --> [*]
```

| Message                                         | Direction                                  | Meaning                                                           |
| ----------------------------------------------- | ------------------------------------------ | ----------------------------------------------------------------- |
| `gen.Node.ApplicationLoad`                      | controller service → Ergo node             | Loads a fresh controller application for the runner attempt.      |
| `gen.Node.ApplicationStart`                     | controller service → Ergo node             | Starts the loaded application.                                    |
| `Application.Seal`                              | controller service → application           | Prevents new writer I/O after a start error or cancellation.      |
| `Service.gracefulStop`, `plugin.MessageStop`    | controller service → supervisor            | Requests graceful supervisor shutdown after context cancellation. |
| `Application.WaitQuiesced`, `Application.Close` | controller service → application resources | Waits for accepted I/O, then closes the SQLite handle.            |
| `gen.Node.ApplicationUnload`                    | controller service → Ergo node             | Unloads the cleaned application.                                  |
| `gen.Node.ApplicationStopForce`                 | controller service → Ergo node             | Forces termination after the 45-second graceful-stop timeout.     |

An application failure is returned to `services.Runner`, which retries its service attempt with exponential delays from 1 second to 60 seconds plus up to 25% jitter. These retries are indefinite while the process context remains live. The runner exposes `blink_runner_service_restarts_total` and `blink_runner_service_restart_delay_seconds`.

## Health and readiness

The process serves two HTTP endpoints on separate ports and separate Prometheus registries.

The health server listens on `:8080` - the port the kubelet probes:

| Endpoint        | Current behavior                                                                                                                                                                                                                             |
| --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/health/live`  | Always HTTP 200 while the health service is serving.                                                                                                                                                                                         |
| `/health/ready` | Always HTTP 200: `cmd/controller` supplies no readiness function.                                                                                                                                                                            |
| `/status`       | JSON, per namespace: every subscribed executor's last heartbeat, committed/ready generation, availability, and drift, as reported by that executor's snapshot supervisor. Queried by a same-node `CallProcessID`, never crosses the cluster. |
| `/metrics`      | Prometheus metrics, including runner restart metrics.                                                                                                                                                                                        |

Radar listens on `RADAR_HOST:RADAR_PORT`, default `0.0.0.0:9090` - `services.Common` carries both for every service, and an empty host binds all interfaces so a scraper outside the pod can reach it (radar's own default is `localhost`):

| Endpoint        | Current behavior                                                                                                                                                                                         |
| --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/metrics`      | Prometheus metrics for every controller namespace, on radar's own registry.                                                                                                                              |
| `/health/ready` | HTTP 503 unless every namespace's `controller-<namespace>` signal is up. Each namespace's supervisor registers and moves its own signal, and expires it on its own after 90 seconds without a heartbeat. |
| `/health/live`  | Ignores the controller signals: they register readiness only.                                                                                                                                            |

Actor availability is an internal runtime status; the kubelet probes never gate on it. An active controller actor reports `ready` only after a complete artifact_scanner result and a loaded, ready writer; otherwise it reports `degraded` or `unavailable`. The signal follows that status upward through `MessageActorStatusChanged`, so a drain or a crashed actor takes it down at once rather than waiting out the heartbeat deadline. Readiness is deliberately not wired to `:8080` or to a liveness probe: the controller Service is how executors resolve the Ergo cluster port, so one degraded namespace must not pull the pod out of that Service, and a controller that cannot reach its database must stop serving snapshots rather than be restarted.

### Metrics

Every series carries a `namespace` label, so one query covers all five controllers, plus a `node` label radar adds itself from the Ergo node name - group by it when comparing pods across a rolling update.

| Metric                                                                                                                                                                                                   | Published by          | Meaning                                                                                                                                                                                                                                                                  |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `blink_controller_availability`                                                                                                                                                                          | actor                 | 0 unavailable, 1 degraded, 2 ready.                                                                                                                                                                                                                                      |
| `blink_controller_generation`, `blink_controller_records`                                                                                                                                                | actor                 | Committed generation and tracked records.                                                                                                                                                                                                                                |
| `blink_controller_subscribers`, `blink_controller_executors`, `blink_controller_executors_drifting`                                                                                                      | actor                 | Subscribed executors, executors reporting convergence, and those behind the committed generation past the drift grace. An executor that has gone silent past the stale threshold and holds no subscription is forgotten, so a rolling update does not inflate the count. |
| `blink_controller_snapshot_commits_total`, `blink_controller_snapshot_writes_total{result}`, `blink_controller_snapshot_write_seconds`                                                                   | actor                 | Generations committed and pushed, write results, and dispatch-to-result latency.                                                                                                                                                                                         |
| `blink_controller_artifact_scans_total{result}`, `blink_controller_worker_restarts_total{worker}`                                                                                                        | actor                 | Scan outcomes and scanner/writer meta restarts.                                                                                                                                                                                                                          |
| `blink_controller_artifact_scan_seconds`, `blink_controller_artifact_specs`, `blink_controller_artifact_binaries`, `blink_controller_artifact_scan_failures_total{stage}`                                | artifact_scanner meta | Scan duration, current index sizes, and files it could not index, by stage.                                                                                                                                                                                              |
| `blink_controller_snapshot_load_seconds`, `blink_controller_snapshot_loads_total{result}`                                                                                                                | snapshot_writer meta  | Startup load of persisted state.                                                                                                                                                                                                                                         |
| `blink_controller_snapshot_write_queue`, `blink_controller_snapshot_write_rejects_total`, `blink_controller_snapshot_write_attempts_total{result}`                                                       | snapshot_writer meta  | Queue depth, writes rejected by a full queue, and individual database attempts including retries.                                                                                                                                                                        |
| `blink_controller_supervisor_lifecycle`, `blink_controller_writer_fences`                                                                                                                                | supervisor            | 0 starting, 1 running, 2 draining, 3 stopping; and the writer I/O fences a drain is still waiting on.                                                                                                                                                                    |
| `blink_controller_child_starts_total`, `blink_controller_child_terminations_total{reason}`                                                                                                               | supervisor            | Controller actor churn under a supervisor that outlives it.                                                                                                                                                                                                              |
| `blink_controller_application_state`, `blink_controller_application_loads_total{result}`, `blink_controller_application_terminations_total{reason}`, `blink_controller_application_closes_total{result}` | application           | 0 stopped, 1 loaded; and load, termination, and resource-close outcomes per runner attempt.                                                                                                                                                                              |

Collectors are registered through `gen.Node`, so radar attributes them to the node core and keeps them across actor and supervisor restarts - which is exactly when the supervisor's and application's own series matter. The application registers them at `Load`; the supervisor registers them once on its first reachable radar tick, which heals a namespace loaded before radar was reachable. No other layer registers anything, so no process's death can delete a collector.

Radar keeps custom collectors in its metrics application's shared registry rather than in the pool worker that accepted the registration, so a worker restart loses nothing and re-registering on every tick would buy nothing. The supervisor instead monitors `radar_metrics` and `radar_health` and re-registers only what a restart of either actually dropped.

The Helm chart scrapes this endpoint and ships a Grafana dashboard over these series - see [the deployment runbook](../../deployments/README.md#monitoring).

## Storage and distribution contract

For a namespace, persistence contains controller records, the last reserved generation, and full snapshots - all in SQLite. Authored YAML remains the source for catalog content; persistence supplies history and restart recovery.

On a changed snapshot the writer, in order:

1. upserts controller records;
2. saves the next generation in storage;
3. saves the aggregate snapshot in storage.

Only once all three succeed does the controller actor push `SnapshotUpdate{Snapshot, Changes, Tombstones}` to every subscriber PID it has registered (`SendImportant`, so remote delivery failure is reported rather than silently dropped). A subscriber that first calls `SubscribeRequest` gets the current committed snapshot in the response - the same acceptance test applies both ways: a snapshot (pushed or returned from `SubscribeRequest`) is applied only if its generation is strictly newer than the one the subscriber already has, so redundant delivery from either path is a no-op. An unchanged plan updates records but skips generation reservation, the aggregate-snapshot write, and any push - there is nothing new to distribute.

## Shutdown and failure behavior

On signal, the runner cancels services. A controller service seals its application, asks its supervisor to drain, waits up to 45 seconds, then forces the Ergo application to stop if needed. Cleanup waits for writer I/O that was accepted before sealing, closes the SQLite handle, then unloads the application. Node shutdown has the same 45-second bound. Subscribers are not explicitly torn down on controller shutdown; each one's own `MonitorPID` on the controller surfaces a `gen.MessageDownPID` and schedules its own resubscribe.

Within an active application, artifact_scanner and writer meta-process failures are retried with a bounded exponential schedule (default 100 ms to 5 s; five scheduled retries). A write itself gets five attempts. An exhausted worker schedule fails its actor instance; the transient supervisor restarts it subject to its restart intensity. Supervisor restart-intensity exhaustion or application
termination ends the application attempt and therefore uses the runner-level retry described above.

## Source references

- `cmd/controller/main.go` - configuration, five service registrations, cluster/etcd wiring, node, signals, health server, and node close.
- `internal/runtime/controller/service.go` - attempt lifecycle, cleanup, and graceful/forced stop.
- `internal/runtime/controller/controller_application.go` - owned SQLite resources and EDF network type registration.
- `internal/runtime/controller/controller_actor.go` - subscriber registration, `notifySubscribers`, and executor drift tracking.
- `internal/runtime/plugin/node.go` - `EtcdClusterConfig`, `NewEtcdRegistrar`, and cluster networking setup shared by every binary.
- `internal/services/runner.go` and `internal/services/health.go` - process retries and HTTP health behavior.
- `internal/backends/{database.go,sql.go,record.go}` - durable persistence and schema.
