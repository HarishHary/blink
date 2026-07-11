# Blink

Blink is an **event-driven detection engine**. Raw events flow through a chain of Kafka topics, each consumed by a small Go microservice that applies one transformation, ending in dispatched alerts. Detection logic lives in **plugins** - each runs as its own subprocess (`hashicorp/go-plugin` over gRPC), so a faulty rule can't take down the pipeline.

## Pipeline

```
events
  → event_matcher     pre-filter: which rules is each event eligible for?
  → rule_executor     evaluate rule plugins, emit alerts
  → alert_merger      dedupe/merge by merge_by_keys within a time window
  → rule_tuner        adjust severity / confidence
  → alert_enricher    add external context
  → alert_formatter   shape for delivery
  → alert_dispatcher  send (Slack, etc.)
```

## Control plane

A separate control plane (`cmd/controller`) reconciles each plugin type's YAML config against a backing store and publishes the effective desired state as **per-ID keyed, log-compacted Kafka snapshots**. Every pipeline pod consumes its type's snapshot and reconciles its plugin subprocesses - no pod reads plugin config off local disk.

Both planes run the **same reaction loop**: a _source_ assembles a `Snapshot` and fires a coalescing signal; a _subscriber_ running alongside it re-reads the latest snapshot and re-syncs.

```
YAML edit -▶ LocalReader --elect--▶ PluginController --▶ ═══ Kafka snapshot topic ═══
             (control plane)         (writer, + DB)             │  broadcast to every pod
                                                                ▼
                               SnapshotReader --signal--▶ PluginExecutor --▶ spawn / kill subprocs
                               (per pod)                   (subscriber)
```

`LocalReader:PluginController` and `SnapshotReader:PluginExecutor` are twins (one `SnapshotSource` interface); each source has exactly one subscriber, and the "many readers" scale-out is the Kafka broadcast across pods - not multiple subscribers. Full code flow and worked examples: **[docs/internals/reconcile-loop.md](docs/internals/reconcile-loop.md)**.

## Plugins

Five plugin types - `rules`, `matchers`, `tuning_rules`, `enrichments`, `formatters` - each defined under `pkg/<type>/` with a Go SDK, and example configs under `examples/<type>/`. Plugins support `blue_green` / `canary` / `shadow` rollout modes.

## Layout

| Path           | What                                                                                 |
| -------------- | ------------------------------------------------------------------------------------ |
| `cmd/`         | the eight services (the seven pipeline stages + `controller`)                        |
| `internal/`    | Kafka broker, control plane, plugin runtime, process pools, config, backends         |
| `pkg/`         | domain types (`events`, `alerts`, `scoring`) + the per-type plugin SDKs              |
| `deployments/` | Kubernetes Helm charts: Kafka cluster + topics, Blink services, and KEDA autoscaling |
| `examples/`    | sample plugin configs                                                                |
| `docs/`        | architecture & internals (below)                                                     |

## Documentation

- **[`docs/internals/message-flow.md`](docs/internals/message-flow.md)** - the authoritative reference for every message hop: schemas, concrete examples, and edge cases.
- **[`docs/internals/performance.md`](docs/internals/performance.md)** - scaling: batching, pools, partitions/pods, `log_type` rollout, autoscaling, DLQ, and the edge cases.
- **[`docs/internals/partitioning.md`](docs/internals/partitioning.md)** - the three "keys" (Kafka partition key vs. `PoolKey` vs. canary hash key), worked examples, and merge co-location.
- **[`docs/internals/reconcile-loop.md`](docs/internals/reconcile-loop.md)** - the subscribe → coalescing-signal → re-read → re-sync loop, mirrored on both planes (`LocalReader`→`PluginController` and `SnapshotReader`→`PluginExecutor`), with the code flow and worked examples.
- [`docs/services/`](docs/services/) - per-service reference. [`docs/internals/`](docs/internals/) - plugin-system internals.

## Getting started

- **[DEVELOPMENT.md](DEVELOPMENT.md)** - prerequisites, local setup, common commands.
- **[deployments/README.md](deployments/README.md)** - running on Kubernetes (Podman/Minikube).
- **[CONTRIBUTING.md](CONTRIBUTING.md)** - how to contribute.

Kubernetes charts share `deployments/helm/values.yaml`: `global.logTypes` controls
per-log-type matcher/executor resources, while `global.sharedStages` defines the
shared alert-stage Deployment, topic, group, optional DLQ, and optional scaler names.
`withDLQ` and `withScaler` control conditional generation; merger and dispatcher
DLQs remain disabled pending runtime support. See
[deployments/README.md](deployments/README.md) for the complete derived-name table.
