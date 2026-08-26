# Plugin YAML sidecar schemas

Every plugin binary `<name>` ships alongside a `<name>.yaml` **sidecar** that declares its identity, rollout mode, and type-specific configuration. The controller's artifact_scanner meta process parses these into the snapshot; see [Controller runtime](../controller-runtime.md#artifact-scanner-meta).

> **The plugin `name` is the sidecar filename stem, not a YAML field.** 
> `Name` is derived from the file at load time (`plugin.PluginMetadata.Name` is `yaml:"-"`), so a `name:` (or `file_name:`) key in the YAML is **ignored**. Rename the file to rename the plugin.

## Common fields (every plugin type)

These come from `plugin.PluginMetadata`, embedded (`yaml:",inline"`) in every type's metadata:

| Field               | Type          | Notes                                                                                    |
| ------------------- | ------------- | ---------------------------------------------------------------------------------------- |
| `id`                | string (UUID) | Stable logical plugin identity. A canary reuses the same `id`.                           |
| `display_name`      | string        | Human-readable name.                                                                     |
| `description`       | string        | Free text.                                                                               |
| `enabled`           | bool          | Whether the plugin runs. `false` stops its subprocesses (no tombstone).                  |
| `version`           | string        | Semver, informational.                                                                   |
| `mode`              | string        | Rollout mode: `blue-green` (default), `canary`, or `shadow`.                             |
| `rollout_pct`       | number        | Canary traffic percentage; used only when `mode: canary`.                                |
| `min_procs`         | int           | Minimum plugin subprocesses.                                                             |
| `max_procs`         | int           | Maximum plugin subprocesses for the actor deployment manager, `<= 100`. Defaults to `1`. |
| `calls_per_process` | int           | Invocations one of those subprocesses may serve at once, `<= 64`. Defaults to `1`.       |

Both process fields should usually stay at their defaults. `max_procs` shards a batch across subprocesses, which splits it into more calls without reducing its work, so extra processes pay only for a plugin whose own per-event CPU dominates per-call overhead, and only when cores are free to run them.

`calls_per_process` is the cheaper form of the same concurrency - a second call inside a subprocess costs a goroutine and a gRPC stream rather than another OS process - but it is the one that needs something from the plugin. **Your plugin must be concurrency-safe before you raise it.** The plugin server holds one plugin object for the whole subprocess and gRPC hands every RPC its own goroutine, so at `calls_per_process: 4` four calls run through the same object at once, and any state it shares between them - maps, cursors, open files, HTTP or database clients that are not safe for concurrent use - is a race. The default `1` is the serialized single call every plugin was written against, and nothing in the runtime can verify that yours is safe for more. Raise it for a plugin that spends its time waiting on something else, one step at a time, and measure.

Rollout modes, deployment routes, and plugin processes are covered in [Plugin runtime](../plugin-runtime.md#deployment-route); [Concurrency knobs](../concurrency-knobs.md) measures what each value costs.

## Per-type schemas

Each type embeds the common fields above and adds its own:

- [rules-schema.md](rules-schema.md) - scoring, rollout, merging, signal, pipeline stages, observables.
- [matchers-schema.md](matchers-schema.md) - pre-filter matchers (`global`).
- [tuning_rules-schema.md](tuning_rules-schema.md) - severity/confidence tuning (`rule_type`, `confidence`, `global`).
- [enrichments-schema.md](enrichments-schema.md) - external context (`depends_on`).
- [formatters-schema.md](formatters-schema.md) - delivery shaping (common fields only).
