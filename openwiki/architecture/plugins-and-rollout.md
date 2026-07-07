# Plugins and Rollout

Detection behavior in Blink is delivered by **plugins**. Each plugin is a separate
subprocess speaking gRPC via `hashicorp/go-plugin`, so a crash or hang in one plugin
cannot take down the host service. This page covers the five plugin types, the SDK
and config contract, and the three rollout modes. Deep references live in
[`/docs/internals/`](../../docs/internals/README.md).

## The five plugin types

Each type is defined under `pkg/<type>/` with a Go SDK plus a generated RPC package,
and has example configs under `examples/<type>/`:

| Type           | Package            | Stage that runs it | Purpose                                                  |
| -------------- | ------------------ | ------------------ | -------------------------------------------------------- |
| `matchers`     | `pkg/matchers`     | `event_matcher`    | Pre-filter: decide which rules an event is eligible for. |
| `rules`        | `pkg/rules`        | `rule_executor`    | Detection: evaluate an event and emit alerts.            |
| `tuning_rules` | `pkg/tuning_rules` | `rule_tuner`       | Adjust an alert's severity/confidence.                   |
| `enrichments`  | `pkg/enrichments`  | `alert_enricher`   | Add external context to an alert.                        |
| `formatters`   | `pkg/formatters`   | `alert_formatter`  | Shape an alert for delivery.                             |

Each `pkg/<type>/` follows the same internal shape (rules is the reference):

- `<type>.go` / `rule.go` — the in-memory metadata type and domain logic.
- `serve.go` — the SDK entry a plugin author calls (`rules.Serve(myRule{})`); defines
  the `Plugin` interface and a `Base<Type>` with no-op defaults.
- `rpc_<type>/` — the generated protobuf/gRPC contract (`*.proto` + `*.pb.go`).
- `rpc_<type>.go` — host-side gRPC client wrapper.
- `adapter.go`, `executor.go`, `loader.go`, `pool.go` — glue to the generic runtime
  (`internal/plugin`, `internal/pools`, `internal/config`).

Reference: [`/docs/internals/README.md`](../../docs/internals/README.md).

## Writing a plugin

A plugin is a standalone Go `main` package that implements the type's `Plugin`
interface and calls `Serve`. Optional capability methods (title, description,
severity, context, merge keys) let a rule shape the alert; returning the zero value
falls back to the YAML-configured default.

Example (`examples/rules/failed-login/main.go`):

```go
type failedLogin struct{ rules.BaseRule }

func (failedLogin) Evaluate(_ context.Context, event events.Event) (bool, errors.Error) {
    action, _ := event["action"].(string)
    status, _ := event["status"].(string)
    return strings.EqualFold(action, "login") && strings.EqualFold(status, "failed"), nil
}

func main() { rules.Serve(failedLogin{}) }
```

Every plugin binary `<name>` ships a `<name>.yaml` **sidecar** declaring identity,
rollout mode, and type-specific config. Important: the plugin `name` is the sidecar
**filename stem**, not a YAML field — a `name:` key is ignored, so rename the file to
rename the plugin. See the failed-login sidecar (`examples/rules/failed-login/rule.yaml`).

### Common config fields

Shared across every type via `plugin.PluginMetadata` (`internal/plugin/types.go`,
embedded `yaml:",inline"`):

| Field                         | Notes                                                          |
| ----------------------------- | -------------------------------------------------------------- |
| `id`                          | Stable logical identity (UUID). A canary reuses the same `id`. |
| `display_name`, `description` | Human-readable metadata.                                       |
| `enabled`                     | `false` stops the plugin's subprocesses (no tombstone).        |
| `version`                     | Semver.                                                        |
| `mode`                        | Rollout mode: `blue-green` (default), `canary`, or `shadow`.   |
| `rollout_pct`                 | Canary traffic percentage (used only when `mode: canary`).     |
| `min_procs` / `max_procs`     | Worker subprocess count bounds.                                |

Per-type fields (scoring, matchers, merge keys, etc.) are documented in
[`/docs/internals/schemas/`](../../docs/internals/schemas/README.md) — start at the
schemas README, then the per-type page (e.g. `rules-schema.md`).

## Rollout modes

Every plugin can be rolled out in one of three modes, defined in
`internal/pools/routing.go` (`RolloutMode`):

- **`blue-green`** (default) — pre-warm the new version's pool, flip the active
  generation atomically, then drain the old pool.
- **`canary`** — route `rollout_pct`% of calls to the new version by consistent hash;
  the rest stay on the old version. Requires an explicit promotion to go 100%.
- **`shadow`** — call the new version in the background, discard its result, and log
  errors. Zero production impact; used to validate a version before promoting it.

The pool machinery that implements this:

- `ProcessPool[T]` (`internal/pools/pool.go`) holds one `VersionedPool` per
  `PoolKey{Id, Name, Hash}`, tracks the `active`/`pending`/`removed` version per
  plugin id, routes each `Call` by rollout mode, and drains on version swap.
- `VersionedPool[T]` (`internal/pools/versioned_pool.go`) is a fixed-size semaphore
  over one binary version's subprocess handles.

Within a single plugin, a per-batch call is fanned across that plugin's `max_procs`
workers: the per-type `pkg/<type>/pool.go` sizes the shard count off the serving
pool and uses `pools.ShardConcurrent` (`internal/pools/shard.go`) to split the batch
into contiguous, order-preserving chunks that each acquire their own subprocess, then
concatenates the results. A single-worker pool (or too few items) takes the un-sharded
single-call path, identical to earlier behavior. See `pkg/rules/pool.go` (`Evaluate`)
and `internal/pools/shard_test.go`.

There are three distinct "keys" in play (Kafka partition key vs. `PoolKey` vs. canary
hash key) — a common source of confusion. See
[`/docs/internals/partitioning.md`](../../docs/internals/partitioning.md) and
[`/docs/internals/plugin-versioning.md`](../../docs/internals/plugin-versioning.md)
for the end-to-end rollout story.

## Where to make changes

- **Author a detection** → new `main` package under `examples/<type>/` (or your own
  location) implementing the `Plugin` interface + a YAML sidecar. No host code change.
- **Add a capability method to a plugin type** → edit the type's `serve.go` interface,
  the `rpc_<type>/*.proto`, regenerate, then wire it through `adapter.go`/`rpc_<type>.go`.
  Verify the base type still provides a sensible default.
- **Change rollout behavior** → `internal/pools`. The rollout modes are covered by
  dedicated table-driven tests (`internal/pools/pool_bluegreen_*`, `pool_canary_*`,
  `pool_shadow_*`, `pool_rapid_shadow_*`); update and run them.
- **Change config validation / election** → `internal/controller/local_reader.go` and
  the per-type `loader.go`; `pkg/rules/validate_*_test.go` shows the expected rules.

See [architecture/overview.md](overview.md) for how plugins are reconciled and
[domain/model.md](../domain/model.md) for the data they operate on.
