# Enrichment sidecar schema

An enrichment binary `<name>` ships alongside `<name>.yaml`. Enrichments add external context to alerts. The sidecar embeds the
[common fields](README.md#common-fields-every-plugin-type) and adds one field (`pkg/enrichments.EnrichmentMetadata`).

## Enrichment-specific fields

| Field        | Type     | Notes                                                                       |
| ------------ | -------- | --------------------------------------------------------------------------- |
| `depends_on` | []string | Names of enrichments that must run before this one (ordering dependencies). |

## Example

```yaml
id: "550e8400-e29b-41d4-a716-446655440000"
display_name: "GeoIP Enrichment"
description: "Adds geographic location data to events."
enabled: true
version: "1.0.0"
mode: "blue-green"
min_procs: 1
max_procs: 2

depends_on: ["other-enrichment"]
```
