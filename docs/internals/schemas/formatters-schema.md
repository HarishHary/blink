# Formatter sidecar schema

A formatter binary `<name>` ships alongside `<name>.yaml`. Formatters shape an alert for delivery. They add **no** type-specific fields - the sidecar is just the [common fields](README.md#common-fields-every-plugin-type) (`pkg/formatters.FormatterMetadata` embeds only `plugin.Spec`).

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
