# Blink services

This index contains only the services currently documented for the Ergo actor runtime.

| Service                           | Input                                           | Output                                                                     | Responsibility                                                                                                    |
| ------------------------------------ | --------------------------------------------------- | -------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| [controller](controller.md)       | Plugin sidecars, binaries, and SQLite state     | `SnapshotUpdate` pushed to subscribers over the Ergo cluster, five namespaces | Runs one control application per plugin type and distributes effective desired state directly to executors.       |
| [event_matcher](event_matcher.md) | Raw JSON events plus matcher and rule snapshots | Protobuf `ExecMessage` records, terminal matcher DLQ records, or no output | Selects candidate rules by `log_type`, evaluates required matcher plugins, and preserves the input key on output. |

## Runtime relationship

The controller is the snapshot distributor. Event matcher is a subscriber: its attempt-owned matcher application subscribes to the matcher namespace's controller actor and its separate projection subscribes to the rule namespace's. Both use local Ergo actors; the native Ergo cluster (backed by etcd for node discovery) is their only cross-process connection - Kafka carries the primary event/alert pipeline, not this.

For actor composition and snapshot ownership, see the [runtime overview](../internals/README.md). For wire contracts, see [message flow](../internals/message-flow.md).
