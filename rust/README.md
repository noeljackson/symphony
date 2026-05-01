# Symphony (Rust)

Rust port of Symphony, the workflow-driven coding-agent orchestrator described
in [`../SPEC.md`](../SPEC.md). The Elixir implementation in `../elixir` is kept
as prior art and reference.

Targets `Core Conformance` (SPEC §18.1) plus the two RECOMMENDED extensions
the Elixir version ships:

- HTTP dashboard / `/api/v1/*` (SPEC §13.7)
- `linear_graphql` client-side tool (SPEC §10.5)

## Layout

| Crate | Responsibility |
|---|---|
| `symphony-core` | Domain model, workflow loader, typed config, prompt rendering, watcher |
| `symphony-tracker` | `Tracker` trait + Linear adapter |
| `symphony-codex` | Codex app-server stdio client |
| `symphony-workspace` | Workspace manager, path safety, hook runner |
| `symphony-orchestrator` | Single-authority actor: dispatch, retries, reconciliation |
| `symphony-http` | Optional dashboard + JSON API |
| `symphony-cli` | `symphony` binary |

## Build

```sh
cargo build --release
cargo test
```
