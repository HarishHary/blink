# Blink — OpenWiki Quickstart

Blink is an **event-driven security detection engine**, written as a Go monorepo.
Raw events flow through a chain of Kafka topics; each topic is consumed by a small
Go microservice that applies exactly one transformation, ending in dispatched
alerts (Slack, etc.). Detection logic lives in **plugins** — each runs as its own
subprocess (`hashicorp/go-plugin` over gRPC), so a faulty rule cannot take down the
pipeline.

This OpenWiki is an **opinionated map and synthesis layer** over the repository. The
project already ships deep reference docs under `/docs`; this wiki orients you, links
to those references, and adds change-oriented guidance for people and agents making
edits. When a topic has an authoritative reference in `/docs`, this wiki links to it
rather than duplicating it.

- Source of truth for the product story: [`/README.md`](../README.md)
- Local setup and commands: [`/DEVELOPMENT.md`](../DEVELOPMENT.md)
- Per-service reference: [`/docs/services/`](../docs/services/README.md)
- Plugin-system internals: [`/docs/internals/`](../docs/internals/README.md)

## The mental model in one minute

There are **two planes**:

- **Data plane** — the pipeline. Seven microservices, one per stage, each reading a
  Kafka topic, transforming, and writing the next topic:

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

- **Control plane** — one binary (`cmd/controller`) that reconciles each plugin
  type's YAML config against a backing store and publishes the effective desired
  state as **per-ID keyed, log-compacted Kafka snapshots**. Every pipeline pod
  consumes its type's snapshot and reconciles its plugin subprocesses. No data-plane
  pod reads plugin config off local disk.

Both planes run the **same reaction loop**: a *source* assembles a `Snapshot` and
fires a coalescing signal; a *subscriber* re-reads the latest snapshot and re-syncs.
See [architecture/overview.md](architecture/overview.md) and
[`/docs/internals/reconcile-loop.md`](../docs/internals/reconcile-loop.md).

Detection behavior is delivered through **five plugin types** — `rules`, `matchers`,
`tuning_rules`, `enrichments`, `formatters` — each with a Go SDK and RPC contract.
See [architecture/plugins-and-rollout.md](architecture/plugins-and-rollout.md).

The data that flows between stages is a small set of domain types (`Event`, `Alert`,
scoring). See [domain/model.md](domain/model.md).

## Repository layout

| Path           | What                                                                                    | Wiki page                                                                                                      |
| -------------- | --------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `cmd/`         | the eight services (seven pipeline stages + `controller`)                               | [architecture/overview.md](architecture/overview.md)                                                           |
| `internal/`    | Kafka brokers, control plane, plugin runtime, process pools, config, backends, snapshot | [architecture/overview.md](architecture/overview.md)                                                           |
| `pkg/`         | domain types (`events`, `alerts`, `scoring`) + the per-type plugin SDKs                 | [domain/model.md](domain/model.md), [architecture/plugins-and-rollout.md](architecture/plugins-and-rollout.md) |
| `deployments/` | Kubernetes: Kafka cluster + topics, per-service manifests, KEDA autoscaling, Helm chart | [operations-and-testing.md](operations-and-testing.md)                                                         |
| `examples/`    | sample plugin configs, one directory per plugin type                                    | [architecture/plugins-and-rollout.md](architecture/plugins-and-rollout.md)                                     |
| `docs/`        | authoritative architecture & internals reference                                        | linked throughout                                                                                              |

Note: the root [`/main.go`](../main.go) is an empty stub (`func main() {}`). The real
entrypoints are the `main.go` files under each `cmd/<service>/` directory.

## Where to start for common changes

- **Add or change a detection rule** → write a rule plugin against the SDK in
  `pkg/rules`; see [architecture/plugins-and-rollout.md](architecture/plugins-and-rollout.md)
  and [`/docs/internals/schemas/rules-schema.md`](../docs/internals/schemas/rules-schema.md).
- **Change how a pipeline stage transforms messages** → edit the relevant service
  under `cmd/<service>/`; per-service reference in [`/docs/services/`](../docs/services/README.md).
- **Change the message contract between stages** → touch `pkg/events`, `pkg/alerts`,
  or the `*.proto`/generated `pb` packages; see [domain/model.md](domain/model.md) and
  [`/docs/internals/message-flow.md`](../docs/internals/message-flow.md).
- **Change plugin lifecycle, rollout, or the reconcile loop** → work in
  `internal/plugin`, `internal/pools`, `internal/controller`; see
  [architecture/overview.md](architecture/overview.md) and
  [`/docs/internals/`](../docs/internals/README.md).
- **Change persistence or transport** → `internal/backends` (SQL / no-db) and
  `internal/brokers` (Kafka / Event Hubs); see
  [operations-and-testing.md](operations-and-testing.md).

## Sections

- [architecture/overview.md](architecture/overview.md) — the two planes, pipeline
  wiring, the reconcile loop, and the generic plugin machinery.
- [architecture/plugins-and-rollout.md](architecture/plugins-and-rollout.md) — the
  five plugin types, the SDK/RPC contract, and blue-green / canary / shadow rollout.
- [domain/model.md](domain/model.md) — `Event`, `Alert`, `RuleMetadata`, and scoring.
- [operations-and-testing.md](operations-and-testing.md) — build/test/lint, Kubernetes
  deployment, backends/brokers, and change-safety guidance.

## Caveats for readers and agents

- The repository was recently through a **large refactor** (see git history around
  commit `b033b1b`). Older concepts — `dispatchers`, `sinks`, `sources`, cloud
  backends (Athena/Snowflake/DynamoDB/Elastic), and the `pluginmgr`/`manager` naming —
  were removed or renamed. Trust current source over any stale mentions; the
  `/docs` tree is generally up to date with the new structure.
- Do not treat [`/TODO.md`](../TODO.md) as current API. It is a scratch/roadmap file.
- Generated protobuf code lives in `*/pb/` and `*/rpc_*/` packages; edit the `.proto`
  and regenerate rather than hand-editing generated `.pb.go` files.
