# Plugin YAML sidecar schemas

Every plugin binary `<name>` ships alongside a `<name>.yaml` **sidecar** that declares its identity, rollout mode, and type-specific configuration. The controller's artifact_scanner meta process parses these into the snapshot; see [Controller runtime](../controller-runtime.md#artifact-scanner-meta).

> **The plugin `name` is the sidecar filename stem, not a YAML field.** 
> `Name` is derived from the file at load time (`plugin.PluginMetadata.Name` is `yaml:"-"`), so a `name:` (or `file_name:`) key in the YAML is **ignored**. Rename the file to rename the plugin.

## Common fields (every plugin type)

These come from `plugin.PluginMetadata`, embedded (`yaml:",inline"`) in every type's metadata:

| Field          | Type          | Notes                                                                                    |
| -------------- | ------------- | ---------------------------------------------------------------------------------------- |
| `id`           | string (UUID) | Stable logical plugin identity. A canary reuses the same `id`.                           |
| `display_name` | string        | Human-readable name.                                                                     |
| `description`  | string        | Free text.                                                                               |
| `enabled`      | bool          | Whether the plugin runs. `false` stops its subprocesses (no tombstone).                  |
| `version`      | string        | Semver, informational.                                                                   |
| `mode`         | string        | Rollout mode: `blue-green` (default), `canary`, or `shadow`.                             |
| `rollout_pct`  | number        | Canary traffic percentage; used only when `mode: canary`.                                |
| `min_procs`    | int           | Minimum plugin subprocesses.                                                             |
| `max_procs`    | int           | Maximum plugin subprocesses for the actor deployment manager, `<= 100`. Defaults to `1`. |

`max_procs` should usually stay at its default: sharding a batch across plugin processes splits it into more calls without reducing its work, so extra processes pay only for a plugin whose own per-event CPU dominates per-call overhead, and only when cores are free to run them. Rollout modes, deployment routes, and plugin processes are covered in [Plugin runtime](../plugin-runtime.md#deployment-route); [Concurrency knobs](../concurrency-knobs.md) measures what each value costs.

## Per-type schemas

Each type embeds the common fields above and adds its own:

- [rules-schema.md](rules-schema.md) - scoring, rollout, merging, signal, pipeline stages, observables.
- [matchers-schema.md](matchers-schema.md) - pre-filter matchers (`global`).
- [tuning_rules-schema.md](tuning_rules-schema.md) - severity/confidence tuning (`rule_type`, `confidence`, `global`).
- [enrichments-schema.md](enrichments-schema.md) - external context (`depends_on`).
- [formatters-schema.md](formatters-schema.md) - delivery shaping (common fields only).
