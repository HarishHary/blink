# Blink

### A pluggable runtime for the next generation of threat detection

Blink is building a threat-detection platform where detection logic, correlation, enrichment, and investigation can evolve independently. Its event-driven Go runtime executes detection capabilities as isolated plugins, moves alerts through focused pipeline stages, and treats testing and safe rollout as part of detection engineering rather than an afterthought.

> [!IMPORTANT]
> Blink is under active development. The event pipeline, Go plugin runtime,
> control plane, rollout modes, and deployment foundation exist today. CEL rules,
> advanced correlation, graph detections, and AI agents are project direction and
> are not yet complete features.

## Vision

Modern detection needs more than a growing collection of static rules. Blink is
designed around five goals:

- **One runtime, multiple detection models.** Run Go plugins today, then add CEL,
  stateful correlation, and graph-based detection without replacing the pipeline.
- **Capabilities are safe delivery units.** Rules, matchers, tuners, enrichments,
  formatters, and future actors are versioned, isolated, tested, and rolled out
  independently.
- **Correlation is a first-class detection layer.** Move from basic alert merging
  toward sequence, composite, aggregation, and graph rules.
- **AI agents are governed pipeline actors.** Agents should investigate and enrich
  alerts under the same contracts, tests, observability, and rollout controls as
  deterministic plugins, not operate as a bolt-on chatbot.
- **The pipeline can keep growing.** New bounded services can join the event flow
  without turning the runtime into a monolith.

## What Blink Is Building

| Area                | Available today                                                       | Project direction                                            |
| ------------------- | --------------------------------------------------------------------- | ------------------------------------------------------------ |
| Detection runtime   | Process-isolated Go rule plugins over gRPC                            | CEL rules alongside compiled plugins                         |
| Event processing    | Kafka-backed matching and rule execution                              | More detection models sharing the same event contracts       |
| Correlation         | Alert merge and deduplication by keys and time window                 | Sequence, composite, aggregation, and graph rules            |
| Alert lifecycle     | Scoring, tuning, enrichment, and formatting stages                    | Additional bounded stages and services                       |
| Intelligent context | Deterministic enrichment plugins                                      | AI agents as first-class investigation and enrichment actors |
| Safe delivery       | Versioned snapshots with blue-green, canary, and shadow rollouts      | A common lifecycle for every new rule and actor type         |
| Operations          | Batching, process pools, DLQ paths, Helm deployment, and KEDA scaling | Continued production hardening as new services arrive        |

## Target Pipeline

```text
Events
  -> Match
  -> Detect              Go plugins today; CEL planned
  -> Correlate           merge/dedup today; sequence, composite,
                         aggregation, and graph rules planned
  -> Tune
  -> Enrich/Investigate  plugins today; governed AI agents planned
  -> Format
  -> Deliver
```

This is the target architecture, not a claim that every stage is complete. Each
stage is a service boundary connected through Kafka, allowing it to scale, fail,
and evolve independently.

## Runtime Foundation

Blink's current foundation focuses on safely running detection logic:

- Plugins run as separate subprocesses using
  [`hashicorp/go-plugin`](https://github.com/hashicorp/go-plugin) over gRPC, so a
  faulty capability does not crash its host service.
- The current SDK supports five plugin families: `rules`, `matchers`,
  `tuning_rules`, `enrichments`, and `formatters`.
- A control plane publishes the desired plugin state through log-compacted Kafka
  snapshots; data-plane services reconcile local subprocesses from that state.
- Process pools support `blue_green`, `canary`, and `shadow` rollout modes for
  controlled changes and offline evaluation.
- The alert pipeline currently matches events, evaluates rules, merges related
  alerts, adjusts scoring, adds context, and formats results.

The detailed service and message contracts live in [`docs/`](docs/) so this page can remain focused on what the project is trying to achieve.

## Delivery And Testing

Detection content is production software. Blink's development and deployment process is built around that assumption:

- Unit and integration tests cover services, domain contracts, and plugin SDKs.
- Plugin integration tests build and execute real plugin subprocesses.
- Race detection, `go vet`, and Staticcheck validate concurrency and code quality.
- Helm linting and rendering validate Kubernetes deployment changes.
- Blue-green, canary, and shadow modes separate plugin rollout from service deployment.
- Helm charts provide Kafka, Blink service, and KEDA autoscaling configuration from one shared stage topology with workload-specific runtime bindings.

See [`DEVELOPMENT.md`](DEVELOPMENT.md) for local validation and [`deployments/README.md`](deployments/README.md) for Kubernetes deployment.

## Quick Start

Blink requires Go 1.26 or newer.

```bash
go test ./...
go build ./...
```

For local prerequisites, plugin test behavior, and Kubernetes setup, follow [`DEVELOPMENT.md`](DEVELOPMENT.md).

## Documentation

- [Architecture and plugin internals](docs/internals/README.md)
- [Message flow and schemas](docs/internals/message-flow.md)
- [Service reference](docs/services/README.md)
- [Performance and scaling](docs/internals/performance.md)
- [Partitioning and routing](docs/internals/partitioning.md)
- [Control-plane reconciliation](docs/internals/reconcile-loop.md)
- [Schema reference](docs/internals/schemas/README.md)
- [Kubernetes deployment](deployments/README.md)
- [Contributing](CONTRIBUTING.md)

## Repository

| Path           | Purpose                                                            |
| -------------- | ------------------------------------------------------------------ |
| `cmd/`         | Pipeline services and the controller                               |
| `internal/`    | Broker, control-plane, plugin-runtime, pool, and backend internals |
| `pkg/`         | Events, alerts, scoring, and public plugin SDKs                    |
| `examples/`    | Example plugin implementations and configuration                   |
| `deployments/` | Helm charts and Kubernetes deployment guidance                     |
| `docs/`        | Architecture, service, schema, and operational references          |

Contributions are welcome. Start with [`CONTRIBUTING.md`](CONTRIBUTING.md).
