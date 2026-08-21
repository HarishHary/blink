# Blink services

This index contains only the services currently documented for the Ergo actor runtime.

| Service                           | Input                                           | Output                                                                     | Responsibility                                                                                                    |
| --------------------------------- | ----------------------------------------------- | -------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| [controller](controller.md)       | Plugin sidecars, binaries, and SQLite state     | Five compacted plugin snapshot topics                                      | Runs one control application per plugin type and publishes effective desired state.                               |
| [event_matcher](event_matcher.md) | Raw JSON events plus matcher and rule snapshots | Protobuf `ExecMessage` records, terminal matcher DLQ records, or no output | Selects candidate rules by `log_type`, evaluates required matcher plugins, and preserves the input key on output. |

## Runtime relationship

The controller is the snapshot writer. Event matcher is a snapshot consumer: its attempt-owned matcher application consumes the matcher snapshot and its separate projection consumes the rule snapshot. Both use local Ergo actors; the Kafka topics are their only cross-process connection.

For actor composition and snapshot ownership, see the [runtime overview](../internals/README.md). For wire contracts, see [message flow](../internals/message-flow.md).
