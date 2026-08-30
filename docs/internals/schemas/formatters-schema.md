# Formatter sidecar schema

[Schemas index](README.md) · [Plugin runtime](../plugin-runtime.md) · [Concurrency knobs](../concurrency-knobs.md)

A formatter binary `<name>` ships alongside `<name>.yaml`. Formatters shape an alert for delivery. The sidecar is just the [common fields](README.md#common-fields-every-plugin-type), because `pkg/formatters.FormatterMetadata` embeds only `plugin.Spec`.

## Formatter-specific fields

None. Formatters add **no** type-specific fields.

## Example

```yaml
id: "550e8400-e29b-41d4-a716-446655440001"
display_name: "JSON Summary Formatter"
description: "Formats alert data as a structured JSON summary."
enabled: true
version: "1.0.0"
mode: "blue-green"
min_procs: 1
max_procs: 2
```
