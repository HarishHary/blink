# Tuning rule sidecar schema

[Schemas index](README.md) · [Plugin runtime](../plugin-runtime.md) · [Concurrency knobs](../concurrency-knobs.md)

A tuning-rule binary `<name>` ships alongside `<name>.yaml`. Tuning rules adjust an alert's severity/confidence, or suppress it. The sidecar embeds the [common fields](README.md#common-fields-every-plugin-type) and adds the fields below (`pkg/tuning_rules.TuningRuleMetadata`).

## Tuning-rule-specific fields

| Field        | Type   | Notes                                                                                                     |
| ------------ | ------ | --------------------------------------------------------------------------------------------------------- |
| `global`     | bool   | When `true`, applies to every rule regardless of a rule's `tuning_rules` list.                            |
| `rule_type`  | string | One of `ignore`, `set_confidence`, `increase_confidence`, `decrease_confidence`.                          |
| `confidence` | string | Meaningful only for the `*_confidence` rule types (e.g. `"0.8"` or `"medium"`); leave empty for `ignore`. |

## Example

```yaml
id: "550e8400-e29b-41d4-a716-446655440003"
display_name: "Noisy Hosts Suppressor"
description: "Ignores alerts from known-noisy infrastructure hosts."
enabled: true
version: "1.0.0"
mode: "blue-green"
min_procs: 1
max_procs: 2

global: false
rule_type: "ignore" # ignore | set_confidence | increase_confidence | decrease_confidence
confidence: "" # only used when rule_type is *_confidence
```
