# Plugin YAML sidecar schemas

[Internals index](../README.md) · [Controller runtime](../controller-runtime.md) · [Plugin runtime](../plugin-runtime.md)

Every plugin binary `<name>` ships alongside a `<name>.yaml` **sidecar**. The sidecar declares the plugin's identity, rollout mode, and type-specific configuration. The controller's artifact_scanner meta process parses these into the snapshot; see [Controller runtime](../controller-runtime.md#artifact-scanner-meta).

> **The plugin `name` is the sidecar filename stem, not a YAML field.**
> `Name` is derived from the file at load time (`plugin.Spec.Name` is `yaml:"-"`), so a `name:` (or `file_name:`) key in the YAML is **ignored**. Rename the file to rename the plugin.

## Common fields (every plugin type)

These come from `plugin.Spec`, embedded (`yaml:",inline"`) in every type's metadata:

| Field               | Type          | Notes                                                                                    |
| ------------------- | ------------- | ---------------------------------------------------------------------------------------- |
| `id`                | string (UUID) | Stable logical plugin identity. A canary reuses the same `id`.                           |
| `display_name`      | string        | Human-readable name.                                                                     |
| `description`       | string        | Free text.                                                                               |
| `enabled`           | bool          | Whether the plugin runs. `false` stops its subprocesses (no tombstone).                  |
| `version`           | string        | Semver, informational.                                                                   |
| `mode`              | string        | Rollout mode: `blue-green` (default), `canary`, or `shadow`.                             |
| `rollout_pct`       | number        | Canary traffic percentage; used only when `mode: canary`.                                |
| `min_procs`         | int           | Subprocesses started before the first call and never shrunk past. Defaults to `0`.       |
| `max_procs`         | int           | Maximum plugin subprocesses for the actor deployment manager, `<= 100`. Defaults to `1`. |
| `calls_per_process` | int           | Invocations one of those subprocesses may serve at once, `<= 64`. Defaults to `32`.      |

`min_procs` is a reservation, not a target. Those processes start before any call arrives and sit outside the shared process budget, so 50 plugins at `min_procs: 4` is 200 subprocesses whatever the budget says.

Leave `min_procs` at `0` unless the first call's cold start matters. At `0` a route still gets one process once a call queues.

`max_procs` shards a batch across subprocesses. That splits the batch into more calls without reducing its work, so extra processes pay off only when the plugin's per-event CPU dominates per-call overhead and cores are free to run them.

`calls_per_process` is the cheaper form of the same concurrency. A second call in a subprocess costs a goroutine and a gRPC stream, not another OS process.

> **Your plugin must be concurrency-safe.** The plugin server holds one plugin object per subprocess, and gRPC gives every RPC its own goroutine. At the default `32`, thirty-two calls run through that one object at once.
>
> Any state shared between them is a race: maps, cursors, open files, HTTP or database clients not safe for concurrent use. The runtime cannot check this for you. A plugin that is not safe sets `calls_per_process: 1` and serializes.

Rollout modes, deployment routes, and plugin processes: [Plugin runtime](../plugin-runtime.md#deployment-route). What each value costs: [Concurrency knobs](../concurrency-knobs.md).

## Per-type schemas

Each type embeds the common fields above and adds its own:

- [rules-schema.md](rules-schema.md) - scoring, rollout, merging, signal, pipeline stages, observables.
- [matchers-schema.md](matchers-schema.md) - pre-filter matchers (`global`).
- [tuning_rules-schema.md](tuning_rules-schema.md) - severity/confidence tuning (`rule_type`, `confidence`, `global`).
- [enrichments-schema.md](enrichments-schema.md) - external context (`depends_on`).
- [formatters-schema.md](formatters-schema.md) - delivery shaping (common fields only).
