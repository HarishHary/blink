# Domain Model

The pipeline transforms a small set of domain types that live in `pkg/`. This page
describes them and how they flow. For the exact wire schema at every hop (including
the protobuf `ExecMessage` envelope and edge cases), the authoritative reference is
[`/docs/internals/message-flow.md`](../../docs/internals/message-flow.md).

## Event

`events.Event` (`pkg/events/event.go`) is the input record:

```go
// Event is a dynamic detection record: a nested map of arbitrary fields.
type Event map[string]any
```

It is deliberately schema-less — a nested `map[string]any` — because raw log/event
data varies by source. The type provides lookup and diffing helpers used across the
pipeline: `DeepGet` (nested key path lookup), `GetFirstKey`, `GetMergedKeys` (pull
out `merge_by_keys` values), `CleanEvent` (drop ignored keys), and `ComputeDiff`
(what a record adds over a common subset — used by merging). Plugins read events as
plain maps (see the `failed-login` example in
[architecture/plugins-and-rollout.md](../architecture/plugins-and-rollout.md)).

## Alert

`alerts.Alert` (`pkg/alerts/alert.go`) wraps a detection `Event` with its rule
reference, scoring, and pipeline metadata as it flows through the later stages:

```go
type Alert struct {
    Id, Cluster        string
    Created, Dispatched time.Time
    Event              events.Event
    Staged             bool               // shadow / non-production alert
    OutputsSent        []string
    EnrichmentsApplied []string
    LogSource, LogType string
    SourceEntity, SourceService string
    Confidence scoring.Confidence
    Severity   scoring.Severity
    Rule                *rules.RuleMetadata
    OverrideMergeByKeys []string
    // ...
}
```

An `Alert` is created by `rule_executor` when a rule matches (`NewAlert`), then
mutated in place as it passes through merge → tune → enrich → format → dispatch.
`MergeByKeys()` returns the effective merge keys, preferring a plugin's
`AlertMergeByKeys` override over the rule's YAML `merge_by_keys`.

## RuleMetadata

`rules.RuleMetadata` (`pkg/rules/rule.go`) is the in-memory representation of a rule
YAML sidecar. It embeds `plugin.PluginMetadata` (the common identity/rollout fields —
see [plugins-and-rollout.md](../architecture/plugins-and-rollout.md)) and adds
rule-specific fields grouped by concern:

- **Scoring**: `severity`, `confidence`, `signal_threshold`.
- **Rollout / matching**: `log_types`, `matchers`, `req_subkeys`.
- **Merging**: `merge_by_keys`, `merge_window_mins` (used by `alert_merger`).
- **Signal**: `signal`.
- **Labelling**: `tags`, `references`, `observables`.
- **Downstream pipeline stages**: `dispatchers`, `formatters`, `enrichments`,
  `tuning_rules` — which plugins should process alerts from this rule.

String scoring fields (`severity: "high"`, etc.) are parsed into typed `scoring`
values by `Load()`. Full field reference:
[`/docs/internals/schemas/rules-schema.md`](../../docs/internals/schemas/rules-schema.md).

`EvalResult` (also in `rule.go`) is the per-event outcome a rule returns; fields
beyond `Matched` are populated only when the plugin implements the corresponding
optional capability interface (Titler, Describer, etc.), and empty means "use the
YAML default".

## Scoring

`pkg/scoring/` defines the small ordered enums used for alert scoring:

| Type         | File            | Values (ordered low → high)                 |
| ------------ | --------------- | ------------------------------------------- |
| `Severity`   | `severity.go`   | `info`, `low`, `medium`, `high`, `critical` |
| `Confidence` | `confidence.go` | (see file)                                  |
| `RiskScore`  | `risk.go`       | `low`, `medium`, `high`, `critical`         |
| `Signal`     | `signal.go`     | signal/threshold logic                      |

Each type is an `int` enum with `String()`, `MarshalText`/JSON support, so it can be
carried in YAML, protobuf payloads, and logs. `rule_tuner` and rule capability
methods (`AlertSeverity`) adjust these values as alerts flow.

## How the types move through the pipeline

```
Event  --event_matcher--▶ Event (tagged with eligible rules)
       --rule_executor--▶ Alert (rule matched → NewAlert)
       --alert_merger---▶ Alert (deduped/merged by merge_by_keys within window)
       --rule_tuner-----▶ Alert (Severity/Confidence adjusted)
       --alert_enricher-▶ Alert (Event enriched, EnrichmentsApplied updated)
       --alert_formatter▶ Alert (formatted payload attached)
       --alert_dispatcher▶ delivered (Slack, etc.)
```

Between services these are carried in a protobuf envelope; edit the `.proto` files
(`pkg/alerts/pb/alert.proto`, `internal/exec/exec.proto`, etc.) and regenerate rather
than hand-editing the `*.pb.go` files. See
[`/docs/internals/message-flow.md`](../../docs/internals/message-flow.md).

## Where to make changes

- **Add a field to alerts/events** → update the domain struct in `pkg/alerts` or
  `pkg/events`, the matching `.proto` + generated `pb`, and the `convert.go` mappers.
- **Add a scoring dimension** → `pkg/scoring`; keep the `String()`/marshal methods
  consistent so YAML/proto/log round-trips hold.
- **Change rule config fields** → `pkg/rules/rule.go` (struct + `Load` parsing) and
  the schema doc; `pkg/rules/spec_test.go` and `validate_*_test.go` guard these.
