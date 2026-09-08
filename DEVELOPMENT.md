# Development

This guide is scoped to the currently documented `controller`, `event_matcher`, and `rule_executor` services and their Ergo runtime.

## Prerequisites

- Go 1.26 or newer (see `go.mod`)
- Kafka for a running service
- Docker or Podman and Minikube for the local Helm path
- Optional: `staticcheck` and `pre-commit`

## Build and test

Run these commands from the repository root:

```bash
go build ./cmd/controller ./cmd/event_matcher ./cmd/rule_executor
go test ./cmd/controller
go test ./cmd/event_matcher/matcher
go test ./cmd/rule_executor/executor
go test ./internal/runtime/controller ./internal/runtime/plugin ./internal/runtime/snapshot
go test ./...
staticcheck ./...
pre-commit run --all-files
```

The focused test commands cover the three composition roots and their current actor runtimes. `go test ./...` remains the repository-wide check.

## Runtime layout

- `cmd/controller` starts one local Ergo node and registers five controller services: rule, matcher, tuning, formatter, and enrichment catalogs.
- `cmd/event_matcher` starts one local Ergo node, a process-owned matcher plugin application, and a rule snapshot projection.
- `cmd/rule_executor` starts one local Ergo node and a process-owned rule plugin application.
- `internal/runtime/controller`, `internal/runtime/plugin`, and `internal/runtime/snapshot` contain the actor implementations.
- Each process exposes `/health/live`, `/health/ready`, and `/metrics` on port 8080. The matcher requires matcher and rule state; the executor requires rule state; the controller health service has no
  extra readiness predicate.

## Required environment

All three processes require `KAFKA_BROKERS`, `ETCD_ENDPOINTS`, and `CLUSTER_COOKIE`. `ENVIRONMENT` is optional and only names the Ergo cluster (`blink-<env>`). `DEBUG=true` raises logging to debug level; `RADAR_ENABLED`, `OBSERVER_ENABLED`, and `MCP_ENABLED` each turn on one node endpoint.

| Process         | Required service-specific variables                                                                                                                                                                                                                                                                     |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `controller`    | `CONTROLLER_DATABASE_DSN`, `RULE_PLUGIN_DIR`, `MATCHER_PLUGIN_DIR`, `TUNER_PLUGIN_DIR`, `FORMATTER_PLUGIN_DIR`, `ENRICHER_PLUGIN_DIR`                                                                                                                                                                        |
| `event_matcher` | `KAFKA_TOPIC_MATCHER`, `KAFKA_GROUP_MATCHER`, `KAFKA_TOPIC_EXECUTOR`, `KAFKA_TOPIC_MATCHER_DLQ`, `MATCHER_PLUGIN_DIR`                                                                                                                                                                                           |
| `rule_executor` | `KAFKA_TOPIC_EXECUTOR`, `KAFKA_GROUP_EXECUTOR`, `KAFKA_TOPIC_MERGER`, `KAFKA_TOPIC_EXECUTOR_DLQ`, `RULE_PLUGIN_DIR`                                                                                                                                                                                             |

Optional matcher settings are `MAX_BATCH_SIZE`, `MAX_CONCURRENT_CALLS`, `MATCHER_TIMEOUT_SEC`, `MATCHER_MAX_ATTEMPTS`, `MATCHER_RETRY_BASE_MS`, and `MATCHER_RETRY_CAP_MS`. Defaults are
respectively 10000, 10, 10 seconds, 3, 100 ms, and 5000 ms.

Optional executor settings are `EXECUTOR_BATCH_SIZE`, `EXECUTOR_CONCURRENCY`, `EXECUTOR_TIMEOUT_SEC`, `EXECUTOR_MAX_ATTEMPTS`, `EXECUTOR_RETRY_BASE_MS`, and `EXECUTOR_RETRY_CAP_MS`. Defaults are
respectively 10000, 10, 10 seconds, 3, 100 ms, and 5000 ms.

## Kubernetes

Use the shared `deployments/helm/values.yaml` with each Helm chart. The current deployment instructions, image builds, and render checks are in [deployment](deployments/README.md).

## References

- [Service index](docs/services/README.md)
- [Controller](docs/services/controller.md)
- [Event matcher](docs/services/event_matcher.md)
- [Rule executor](docs/services/rule_executor.md)
- [Runtime overview](docs/internals/README.md)
- [Message flow](docs/internals/message-flow.md)
- [Schema reference](docs/internals/schemas/README.md)
