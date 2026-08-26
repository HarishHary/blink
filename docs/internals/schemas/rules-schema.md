# Rule sidecar schema

A rule binary `<name>` ships alongside `<name>.yaml`. It embeds the [common fields](README.md#common-fields-every-plugin-type) and adds the fields below (`pkg/rules.RuleMetadata`).

## Rule-specific fields

| Field               | Type         | Notes                                                                                      |
| ------------------- | ------------ | ------------------------------------------------------------------------------------------ |
| `severity`          | string       | Base severity (e.g. `low`/`medium`/`high`/`critical`).                                     |
| `confidence`        | string       | Base confidence.                                                                           |
| `signal_threshold`  | string       | Confidence threshold at/above which a match is treated as a signal.                        |
| `log_types`         | []string     | Log types this rule applies to. **Empty = all log types.** Drives `event_matcher` rollout. |
| `matchers`          | []string     | Matcher plugin names that must all pass for an event to be eligible.                       |
| `req_subkeys`       | []string     | Event subkeys that must be present for the rule to evaluate.                               |
| `merge_by_keys`     | []string     | Event fields whose values group alerts into one merged alert (the merge key).              |
| `merge_window_mins` | uint32       | Merge window in minutes; alerts sharing the merge key within it are merged.                |
| `signal`            | bool         | Whether matches emit a signal.                                                             |
| `tags`              | []string     | Labels (e.g. MITRE technique IDs).                                                         |
| `references`        | []string     | URLs / references.                                                                         |
| `observables`       | []Observable | Static fields the rule surfaces in generated alerts (see below).                           |
| `dispatchers`       | []string     | Dispatcher names to deliver alerts to.                                                     |
| `formatters`        | []string     | Formatter plugin names to shape the alert.                                                 |
| `enrichments`       | []string     | Enrichment plugin names to add context.                                                    |
| `tuning_rules`      | []string     | Tuning-rule plugin names to adjust severity/confidence.                                    |

`Observable` (each entry): `name` (string), `description` (string), `aggregation` (bool).

## Example

```yaml
id: "550e8400-e29b-41d4-a716-446655440000"
display_name: "Brute Force Login Attempt"
description: "Detects repeated failed login attempts from a single source."
enabled: true
version: "1.2.0"
mode: "blue-green"
min_procs: 1
max_procs: 2

severity: "high"
confidence: "medium"
signal: true
signal_threshold: "medium"
log_types: ["auth", "cloudtrail"]
matchers: ["prod-accounts"]
req_subkeys: ["source_ip"]
merge_by_keys: ["source_ip", "username"]
merge_window_mins: 60
tags: ["t1078", "initial-access"]
references: ["https://attack.mitre.org/techniques/T1110/"]
observables:
    - name: "source_ip"
      description: "Originating IP"
      aggregation: true
dispatchers: ["pagerduty", "slack"]
formatters: ["json-summary"]
enrichments: ["geoip"]
tuning_rules: ["noisy-hosts"]
```
