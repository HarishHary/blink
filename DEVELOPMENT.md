# Development

This guide is scoped to the currently documented `controller` and `event_matcher` services and their Ergo runtime.

## Prerequisites

- Go 1.26 or newer (see `go.mod`)
- Kafka for a running service
- Docker or Podman and Minikube for the local Helm path
- Optional: `staticcheck` and `pre-commit`

## Build and test

Run these commands from the repository root:

```bash
go build ./cmd/controller ./cmd/event_matcher
go test ./cmd/controller
go test ./cmd/event_matcher/matcher
go test ./internal/runtime/controller ./internal/runtime/plugin ./internal/runtime/snapshot
go test ./...
staticcheck ./...
pre-commit run --all-files
```

The focused test commands cover the two composition roots and their current actor runtimes. `go test ./...` remains the repository-wide check.

## Runtime layout

- `cmd/controller` starts one local Ergo node and registers five controller services: rule, matcher, tuning, formatter, and enrichment catalogs.
- `cmd/event_matcher` starts one local Ergo node, an attempt-owned matcher plugin application, and a rule snapshot projection.
- `internal/runtime/controller`, `internal/runtime/plugin`, and `internal/runtime/snapshot` contain the actor implementations.
- Each process exposes `/health/live`, `/health/ready`, and `/metrics` on port 8080. The matcher readiness check requires both current projections to be ready; the controller health service has no
  extra readiness predicate.

## Required environment

All processes require `KAFKA_BROKERS`; `ENVIRONMENT` is optional and selects logging/runtime diagnostics (`dev` enables the local Ergo observer and MCP applications).

| Process         | Required service-specific variables                                                                                                                                                                                                                                                                     |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `controller`    | `CONTROLLER_DATABASE_DSN`, `KAFKA_TOPIC_EXECUTOR_SNAPSHOT`, `KAFKA_TOPIC_MATCHER_SNAPSHOT`, `KAFKA_TOPIC_TUNER_SNAPSHOT`, `KAFKA_TOPIC_FORMATTER_SNAPSHOT`, `KAFKA_TOPIC_ENRICHER_SNAPSHOT`, `RULE_PLUGIN_DIR`, `MATCHER_PLUGIN_DIR`, `TUNER_PLUGIN_DIR`, `FORMATTER_PLUGIN_DIR`, `ENRICHER_PLUGIN_DIR` |
| `event_matcher` | `KAFKA_TOPIC_MATCHER`, `KAFKA_GROUP_MATCHER`, `KAFKA_TOPIC_EXECUTOR`, `KAFKA_TOPIC_MATCHER_DLQ`, `KAFKA_TOPIC_MATCHER_SNAPSHOT`, `KAFKA_TOPIC_EXECUTOR_SNAPSHOT`, `MATCHER_PLUGIN_DIR`                                                                                                                  |

Optional matcher settings are `MATCHER_BATCH_SIZE`, `MATCHER_CONCURRENCY`, `MATCHER_TIMEOUT_SEC`, `MATCHER_MAX_ATTEMPTS`, `MATCHER_RETRY_BASE_MS`, and `MATCHER_RETRY_CAP_MS`. Defaults are
respectively 50, 8, 10 seconds, 3, 100 ms, and 5000 ms.

## Kubernetes

Use the shared `deployments/helm/values.yaml` with each Helm chart. The current deployment instructions, image builds, and render checks are in [deployment](deployments/README.md).

## References

- [Service index](docs/services/README.md)
- [Runtime overview](docs/internals/README.md)
- [Message flow](docs/internals/message-flow.md)
- [Schema reference](docs/internals/schemas/README.md)
