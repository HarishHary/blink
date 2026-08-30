# Matcher sidecar schema

[Schemas index](README.md) · [Plugin runtime](../plugin-runtime.md) · [Concurrency knobs](../concurrency-knobs.md)

A matcher binary `<name>` ships alongside `<name>.yaml`. Matchers pre-filter events in `event_matcher` and decide which rules an event is eligible for. The sidecar embeds the [common fields](README.md#common-fields-every-plugin-type) and adds the field below (`pkg/matchers.MatcherMetadata`).

## Matcher-specific fields

| Field    | Type | Notes                                                                                    |
| -------- | ---- | ---------------------------------------------------------------------------------------- |
| `global` | bool | When `true`, the matcher applies to every rule regardless of the rule's `matchers` list. |

## Example

```yaml
id: "550e8400-e29b-41d4-a716-446655440002"
display_name: "Production Accounts Matcher"
description: "Matches events from production AWS accounts."
enabled: true
version: "1.0.0"
mode: "blue-green"
min_procs: 1
max_procs: 2

global: false
```
