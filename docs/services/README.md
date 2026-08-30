# Blink services

| Service                           | Input                                       | Output                                                          | Responsibility                                                                            |
| --------------------------------- | ------------------------------------------- | --------------------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| [Controller](controller.md)       | Plugin sidecars, binaries, SQLite state     | `SnapshotUpdate` pushed to cluster subscribers, five namespaces | One control application per plugin type; distributes desired state to executors.          |
| [Event matcher](event_matcher.md) | Raw JSON events, matcher and rule snapshots | Protobuf `ExecMessage` records, matcher DLQ records, or none    | Selects rules by `log_type`, evaluates required matcher plugins, preserves the input key. |

## Runtime relationship

The controller distributes snapshots. Event matcher subscribes: its matcher application to the matcher namespace's controller actor, a separate projection to the rule namespace's. Both use local Ergo actors; the Ergo cluster (etcd for discovery) is their only cross-process connection. Kafka carries the event and alert pipeline.

See the [runtime overview](../internals/README.md) for actor composition, and [message flow](../internals/message-flow.md) for wire contracts.
