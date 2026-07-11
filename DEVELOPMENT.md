# Development

This guide contains useful information for developing on the Blink project. For _what_ Blink
is and how messages flow, start with [README.md](README.md) and
[docs/internals/message-flow.md](docs/internals/message-flow.md).

- [Development](#development)
    - [Overview](#overview)
    - [Prerequisites](#prerequisites)
    - [Components](#components)
    - [Common commands](#common-commands)
    - [Running on Kubernetes](#running-on-kubernetes)
    - [IDEs \& Dev Containers](#ides-dev-containers)
    - [Formatting \& hooks](#formatting-hooks)
    - [Contributing](#contributing)

## Overview

Blink is a Go monorepo: an event-driven detection pipeline of small services wired over Kafka,
with detection logic running in `hashicorp/go-plugin` subprocesses (gRPC). Supporting code is
Bash (`scripts/`) and YAML/Helm (Kubernetes manifests under `deployments/`).

A separate control plane (`cmd/controller`) turns each plugin type's YAML into an effective
`Snapshot` and publishes it on a log-compacted Kafka topic; every pipeline pod consumes its type's
snapshot and reconciles its subprocesses. Both planes react to change with the same
subscribe → signal → re-read → re-sync loop - see
[docs/internals/reconcile-loop.md](docs/internals/reconcile-loop.md).

## Prerequisites

- [Go](https://go.dev/doc/install) **>= 1.26** (see `go.mod`)
- [Docker](https://docs.docker.com/get-docker/) or [Podman](https://podman.io/) - containers and a local Kafka
- [Minikube](https://minikube.sigs.k8s.io/) - local Kubernetes (see [deployments/README.md](deployments/README.md))
- [pre-commit](https://pre-commit.com/) - git hooks
- Optional: VS Code + the Dev Containers extension (a `.devcontainer` is provided)

## Components

| Path                                                                             | What                                                                                                                                                                            |
| -------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cmd/`                                                                           | the eight services: `event_matcher`, `rule_executor`, `alert_merger`, `rule_tuner`, `alert_enricher`, `alert_formatter`, `alert_dispatcher`, and the control plane `controller` |
| `internal/brokers`                                                               | Kafka broker abstraction (`Reader`/`Writer`/`Broker`); also an Event Hubs implementation                                                                                        |
| `internal/controller`                                                            | control plane: `LocalReader[T]` (disk parse+elect → `Snapshot`), `PluginController[T]` (writer), `SnapshotReader` (per-pod reader)                                              |
| `internal/plugin`, `internal/pools`                                              | generic plugin runtime (`PluginExecutor[T]`) and process pools (`ProcessPool[T]`)                                                                                               |
| `internal/config`, `internal/services`, `internal/backends`, `internal/snapshot` | plugin config `Loader[T]`/`Source[T]`/`SnapshotConfig[T]`, service runner/config loader, persistence adapters, snapshot wire model                                              |
| `pkg/{events,alerts,scoring}`                                                    | domain types                                                                                                                                                                    |
| `pkg/{rules,matchers,tuning_rules,enrichments,formatters}`                       | per-type plugin interfaces, RPC, and SDKs                                                                                                                                       |
| `deployments/`                                                                   | Kubernetes Helm charts: `helm/kafka/` (cluster + topics), `helm/blink/` (services + config), `helm/keda/` (Kafka-lag scalers)                                                   |
| `examples/`                                                                      | sample plugin configs, one directory per plugin type                                                                                                                            |
| `docs/`                                                                          | architecture & internals - `docs/internals/message-flow.md` is the message-schema reference                                                                                     |

## Common commands

```bash
go build ./...                       # build everything
go build ./cmd/rule_executor         # build one service
go test ./...                        # run all tests
go test ./pkg/rules/...              # tests for one package
go test ./pkg/rules -run TestName    # a single test by name
staticcheck ./...                    # static analysis (matches CI)
pre-commit run --all-files           # hooks: trailing whitespace, YAML lint, commit-msg
```

Some tests build plugin binaries at runtime (e.g. `pkg/rules/executor_test.go`), so a Go
toolchain must be on `PATH`. If a test fails with a missing-binary error, look for a
`testdata/` directory with a `go build` step in the test setup.

## Running on Kubernetes

Local clusters run on Podman or Minikube. See **[deployments/README.md](deployments/README.md)**
for the Strimzi and KEDA operator prerequisites, chart validation, and install sequence.
Install the independent Helm charts in order: `deployments/helm/kafka`, then
`deployments/helm/blink`, then `deployments/helm/keda`; every chart command passes
`-f deployments/helm/values.yaml`, where `global.logTypes` and the shared alert-stage
topology (`global.sharedStages`) are defined once. `withDLQ` conditionally creates a
stage DLQ and `withScaler` conditionally creates its KEDA ScaledObject; merger and
dispatcher DLQs remain disabled pending runtime support. See the derived names and
the canonical commands in [deployments/README.md](deployments/README.md).

## IDEs & Dev Containers

Use any editor you like. The team uses VS Code, and a Dev Container is provided to give a
ready-made environment: with Docker installed, open the repo in VS Code and install the
[Dev Containers extension](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers),
then "Reopen in Container". See
[Developing inside a Container](https://code.visualstudio.com/docs/devcontainers/containers).

## Formatting & hooks

Install the git hooks once so style/lint issues are caught before review:

```bash
pre-commit install
```

Run `gofmt` and `staticcheck ./...` before opening a PR.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
