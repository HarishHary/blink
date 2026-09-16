# Blink services

| Service                           | Input                                       | Output                                                          | Responsibility                                                                            |
| --------------------------------- | ------------------------------------------- | --------------------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| [Controller](controller.md)       | Plugin sidecars, binaries, SQLite state     | `SnapshotUpdate` pushed to cluster subscribers, five namespaces | One control application per plugin type; distributes desired state to executors.          |
| [Event matcher](event_matcher.md) | Raw JSON events, matcher and rule snapshots | Protobuf `ExecMessage` records, matcher DLQ records, or none    | Selects rules by `log_type`, evaluates required matcher plugins, preserves the input key. |
| [Rule executor](rule_executor.md) | `ExecMessage` records and rule snapshots    | Alerts, executor DLQ records, or none                           | Evaluates selected rule plugins and keys matched alerts for downstream merging.           |
| [Rule tuner](rule_tuner.md)       | Alerts and tuning snapshots                 | Tuned alerts, tuner DLQ records, or an ignored terminal         | Applies global and explicit tuning rules; preserves the source key on output alerts.      |

## Runtime relationship

The controller distributes snapshots. Event matcher subscribes to matcher and rule namespaces; rule executor and rule tuner subscribe to rule and tuning respectively. All three run local Ergo plugin applications, and the Ergo cluster (etcd for discovery) is their only cross-process control-plane connection. Kafka carries the event and alert pipeline.

See the [runtime overview](../internals/README.md) for actor composition, and [message flow](../internals/message-flow.md) for wire contracts.

## Shared metrics

Every service embeds `internal/services.Runner`, so every process exposes `blink_runner_*` on its own health server at `:8080/metrics`. Matcher, executor, and tuner share the Kafka-stage metric contract below, under separate service prefixes. Controller retains its control-plane metrics on radar. Merger, enricher, and formatter do not follow that contract: they expose their own `blink_alert_merger_*`, `blink_alert_enricher_*`, and `blink_alert_formatter_*` families, so do not query them with the suffixes below.

| Metric                                                | Meaning                                                                      |
| ----------------------------------------------------- | ---------------------------------------------------------------------------- |
| `blink_runner_service_restarts_total{service}`        | Restarts of one registered service. A context cancellation is not a restart. |
| `blink_runner_service_restart_delay_seconds{service}` | Backoff actually waited: 1 s doubling to a 60 s cap, plus up to 25%.         |

The `service` label is the service's own `Name()`, not the process:

| Process           | `service` values                                                                                                                |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `controller`      | `controller-rule`, `controller-matcher`, `controller-tuning`, `controller-formatter`, `controller-enrichment`, `health service` |
| `event_matcher`   | `event-matcher`, `health service`                                                                                               |
| `rule_executor`   | `rule-executor`, `health service`                                                                                               |
| `rule_tuner`      | `rule-tuner`, `health service`                                                                                                  |
| `alert_merger`    | `alert-merger`, `health service`                                                                                                |
| `alert_enricher`  | `alert-enricher`, `health service`                                                                                              |
| `alert_formatter` | `alert-formatter`, `health service`                                                                                             |

## Kafka-stage metrics

Event matcher, rule executor, and rule tuner expose the same 19-family baseline on `:8080/metrics`. Prefix each suffix below with `blink_event_matcher_`, `blink_rule_executor_`, or `blink_rule_tuner_`.

| Suffix and labels                       | Type      | Measurement                                                                                                                                  |
| --------------------------------------- | --------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `events_in_total`                       | Counter   | Records returned by successful fetches, including invalid records; internal batch replay does not recount input.                             |
| `batch_size`                            | Histogram | Record count per successful fetch, including empty fetches.                                                                                  |
| `read_batch_total{result}`              | Counter   | Completed broker read attempts by outcome.                                                                                                   |
| `read_batch_seconds`                    | Histogram | Duration of each broker read attempt.                                                                                                        |
| `batch_processing_total{result}`        | Counter   | Completed fetched-batch resolutions by outcome, including state reads, evaluation, retries, preparation, and publication.                    |
| `batch_processing_seconds`              | Histogram | Batch resolution duration, including retry backoff but excluding fetch and commit. Matcher generation replays remain within one observation. |
| `commit_total{result}`                  | Counter   | Completed source-offset commit attempts by outcome.                                                                                          |
| `commit_seconds`                        | Histogram | Duration of each commit attempt.                                                                                                             |
| `evaluation_total{plugin,result}`       | Counter   | Completed admitted runtime calls; a call with any failed item has outcome `error`.                                                           |
| `evaluation_seconds{plugin}`            | Histogram | Runtime call duration, excluding semaphore wait and retry backoff.                                                                           |
| `evaluation_items_total{plugin,result}` | Counter   | Item attempts by `matched`, `unmatched`, or `error`; a whole-call or result-shape failure counts every submitted item as an error.           |
| `evaluation_retries_total{plugin}`      | Counter   | Additional runtime calls actually admitted after the first call in a retry loop.                                                             |
| `evaluations_in_flight`                 | Gauge     | Runtime calls holding service concurrency permits.                                                                                           |
| `write_total{destination,result}`       | Counter   | Completed broker write attempts, not record counts.                                                                                          |
| `write_seconds{destination}`            | Histogram | Duration of each broker write attempt, excluding retry backoff.                                                                              |
| `write_retries_total{destination}`      | Counter   | Additional broker write calls actually attempted after the first in a retry loop.                                                            |
| `records_out_total{destination}`        | Counter   | Records acknowledged by successful broker writes, not prepared records or committed inputs.                                                  |
| `dlq_records_total{stage}`              | Counter   | Acknowledged dead-letter records broken down by processing stage; a subset of `records_out_total{destination="dlq"}`.                        |
| `drops_total{scope,reason}`             | Counter   | Decisions producing no output record; `event` counts source-input decisions, `rule` counts per-rule decisions.                               |

Label conventions:

- Operation `result` is `ok`, `error`, or `canceled`. A failed operation with a canceled service context is `canceled`; a successful operation remains `ok`. A plugin timeout while the service context remains live is `error`.
- Item `result` is `matched`, `unmatched`, or `error`, independently of the aggregate call outcome. Retried items add new observations; these are not unique-event counts.
- `destination` is `output` or `dlq`. Output means the executor topic for matcher, merger topic for executor, and enricher topic for tuner.
- `plugin` is the configured matcher or rule name, never an event identifier. Names determine series cardinality; avoid unbounded plugin-name churn.
- `stage` and drop `reason` are fixed processing categories listed on each service page, never error text, source keys, or rule IDs.

All operation-duration histograms include successful, failed, and canceled attempts. Duration buckets are `0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60` seconds.
Batch-size buckets are `0, 1, 10, 50, 100, 500, 1000, 5000, 10000, 50000` records; Prometheus also supplies the `+Inf` bucket.

Acknowledgment is not an offset commit: partial publication followed by failure can be replayed and counted again. Drop decisions can also belong to uncommitted or internally replayed batches. Do not sum drops across scopes or use input minus output as a loss counter: executor fans one source out to multiple rule outcomes, while tuner can emit a semantic pass-through. Histogram `_count` series provide attempt denominators; counters expose outcomes separately.
