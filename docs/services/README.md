# Blink services

| Service                           | Input                                       | Output                                                          | Responsibility                                                                            |
| --------------------------------- | ------------------------------------------- | --------------------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| [Controller](controller.md)       | Plugin sidecars, binaries, SQLite state     | `SnapshotUpdate` pushed to cluster subscribers, five namespaces | One control application per plugin type; distributes desired state to executors.          |
| [Event matcher](event_matcher.md) | Raw JSON events, matcher and rule snapshots | Protobuf `ExecMessage` records, matcher DLQ records, or none    | Selects rules by `log_type`, evaluates required matcher plugins, preserves the input key. |

## Runtime relationship

The controller distributes snapshots. Event matcher subscribes: its matcher application to the matcher namespace's controller actor, a separate projection to the rule namespace's. Both use local Ergo actors; the Ergo cluster (etcd for discovery) is their only cross-process connection. Kafka carries the event and alert pipeline.

See the [runtime overview](../internals/README.md) for actor composition, and [message flow](../internals/message-flow.md) for wire contracts.

## Shared metrics

Every service embeds `internal/services.Runner`, so every process exposes `blink_runner_*` on its own health server at `:8080/metrics`. These are the only series common to all services; the rest are per-service and documented on each page.

| Metric                                                | Meaning                                                                              |
| ----------------------------------------------------- | ------------------------------------------------------------------------------------ |
| `blink_runner_service_restarts_total{service}`         | Restarts of one registered service. A context cancellation is not a restart.         |
| `blink_runner_service_restart_delay_seconds{service}`  | Backoff actually waited: 1 s doubling to a 60 s cap, plus up to 25%.                 |

The `service` label is the service's own `Name()`, not the process:

| Process         | `service` values                                                                                                            |
| --------------- | --------------------------------------------------------------------------------------------------------------------------- |
| `controller`    | `controller-rule`, `controller-matcher`, `controller-tuning`, `controller-formatter`, `controller-enrichment`, `health service` |
| `event_matcher` | `event-matcher`, `health service`                                                                                           |
