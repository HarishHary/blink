# Operations and Testing

How to build, test, run, and deploy Blink, and where the tests live. The canonical
sources are [`/DEVELOPMENT.md`](../DEVELOPMENT.md) and
[`/deployments/README.md`](../deployments/README.md); this page summarizes and adds
change-oriented guidance.

## Build, test, lint

Blink is a Go monorepo requiring **Go >= 1.26** (see `go.mod`). Common commands
(from [`/DEVELOPMENT.md`](../DEVELOPMENT.md)):

```bash
go build ./...                       # build everything
go build ./cmd/rule_executor         # build one service
go test ./...                        # run all tests
go test ./pkg/rules/...              # tests for one package
go test ./pkg/rules -run TestName    # a single test by name
staticcheck ./...                    # static analysis (matches CI)
pre-commit run --all-files           # hooks: whitespace, YAML lint, commit-msg
```

Note: `main.go` at the repo root is an empty stub (`func main() {}`); real entrypoints
are under `cmd/`. The eight binaries are the seven pipeline stages plus `controller`.

Some tests **build plugin binaries at runtime** (e.g. `pkg/rules/executor_test.go`,
which compiles fixtures under `pkg/rules/testdata/`), so a Go toolchain must be on
`PATH`. A missing-binary test failure usually means a `testdata/` `go build` step
couldn't run.

Install git hooks once so lint/style issues are caught before review:
`pre-commit install`.

## Where the tests are (and what to run when changing an area)

| Area you change                          | Key tests                                                                                                                     | Location                        |
| ---------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- | ------------------------------- |
| Rollout modes (blue-green/canary/shadow) | `pool_bluegreen_promotes_test.go`, `pool_canary_routing_test.go`, `pool_shadow_*_test.go`, `pool_rapid_shadow_deploy_test.go` | `internal/pools/`               |
| Sharding / max-procs                     | `shard_test.go`, `pkg/rules/pool_shard_test.go`                                                                               | `internal/pools/`, `pkg/rules/` |
| Control-plane reconcile                  | `reconcile_test.go`, `local_reader_test.go`, `snapshot_reader_test.go`                                                        | `internal/controller/`          |
| Plugin lifecycle / executor              | `executor_test.go`, `executor_crash_restart_test.go`, `executor_disabled_test.go`, `executor_missing_id_test.go`              | `pkg/rules/`                    |
| Rule config validation                   | `validate_*_test.go`, `spec_test.go`                                                                                          | `pkg/rules/`                    |
| Persistence backends                     | `database_test.go`                                                                                                            | `internal/backends/`            |
| Service config loading                   | `config_loader_test.go`, `sync_test.go`                                                                                       | `internal/services/`            |

CI runs static analysis via `.github/workflows/static-analysis.yml`; run
`gofmt` and `staticcheck ./...` before opening a PR.

## Running on Kubernetes

Local clusters run on Podman or Minikube. Full prerequisites (Strimzi Kafka operator,
KEDA operator) and the apply sequence are in
[`/deployments/README.md`](../deployments/README.md). In outline:

```bash
kubectl apply -f deployments/kafka/    # Kafka cluster + topics (wait for Ready)
kubectl apply -f deployments/blink/    # services + shared config
kubectl apply -f deployments/keda/     # autoscaling (optional)
```

A Helm chart is available under `deployments/helm/blink`. Layout:

- `deployments/kafka/` — Kafka cluster + topic definitions.
- `deployments/blink/` — per-service manifests + shared ConfigMap/Secret.
- `deployments/keda/` — KEDA `ScaledObject`s for autoscaling on consumer lag.
- `deployments/helm/` — Helm chart.

Plugin binaries: services mount a plugin directory as the **binary artifact store**
(the `emptyDir` in the manifests means no plugins load at startup — services pass
events through). Replace with a `PersistentVolumeClaim` or `initContainer` to populate
real plugin binaries. Config no longer comes from this directory — it comes from the
controller's snapshot topic (see [architecture/overview.md](architecture/overview.md)).

## Transport and persistence config

- **Brokers** (`internal/brokers`): Kafka is the default; an Azure Event Hubs
  implementation exists (`eventhub.go`). Topic names and broker addresses are supplied
  via environment/config (`KAFKA_TOPIC_*`, etc.) — see the deployment ConfigMap.
- **Backends** (`internal/backends`): the control plane persists to a `Database`
  (SQLite/Postgres via `sql.go`, or the no-op `nodb.go`). Do **not** read or commit
  secret values; connection strings/credentials belong in Kubernetes Secrets and are
  not documented here.

> Security: never read or commit `.env` files, credentials, tokens, or private keys.
> Sample/placeholder config only.

## Scaling and performance

Batching, process pools, partitions vs. pods, `log_type`-based rollout, KEDA
autoscaling, and the DLQ are covered in depth in
[`/docs/internals/performance.md`](../docs/internals/performance.md) and
[`/docs/internals/batching-and-concurrency.md`](../docs/internals/batching-and-concurrency.md).

## Roadmap / known gaps

`/TODO.md` tracks planned work (e.g. local rule-engine testing harness, global tuning
rules, correlation/signal rules, a UI). Some `deployments/README.md` steps are marked
`FIXME`; treat those as incomplete rather than authoritative.
