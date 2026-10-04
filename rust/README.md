# Symphony (Rust)

Rust port of Symphony, the workflow-driven coding-agent orchestrator described
in [`../SPEC.md`](../SPEC.md). Rust is the only implementation shipped in
this repository.

This crate targets [SPEC v2](../SPEC.md) — the multi-backend revision. The
orchestration core (polling, dispatch, retries, reconciliation, workspace
isolation, hooks, observability) is fully backend-agnostic; the agent
runner is plugged in via `agent.backend` in `WORKFLOW.md`.

## Backend support

| `agent.backend` | Status |
|---|---|
| `codex` | Implemented — `symphony-codex` |
| `claude_code` | Implemented — `symphony-claude-code` |
| `openai_compat` | TBD — covers OpenAI, Moonshot Kimi K2, Zhipu GLM, DeepSeek, vLLM, llama.cpp servers |
| `anthropic_messages` | TBD — Anthropic Messages API |

`RealWorker` dispatches to the right client based on `agent.backend`. Both
implemented backends share the line-delimited JSON `Channel` + `RuntimeEvent`
+ `ToolExecutor` plumbing from `symphony-codex`; only the wire format and
session lifecycle differ.

## Crate layout

| Crate | Responsibility |
|---|---|
| `symphony-core` | Domain model, workflow loader, typed config, prompt rendering, watcher |
| `symphony-tracker` | `Tracker` trait + Linear adapter + `linear_graphql` tool |
| `symphony-codex` | Codex stdio app-server backend |
| `symphony-claude-code` | Claude Code stdio backend (stream-json mode) |
| `symphony-workspace` | Workspace manager, path safety, hook runner |
| `symphony-orchestrator` | Single-authority actor: dispatch, retries, reconciliation, `RealWorker` |
| `symphony-http` | Optional dashboard + JSON API |
| `symphony-cli` | `symphony` binary |

## Build

```sh
cargo build --release --locked
cargo test --workspace --locked
```

CI: `.github/workflows/rust.yml` runs `cargo fmt --check`, `cargo clippy
--workspace --all-targets --locked -- -D warnings`, and `cargo test
--workspace --locked` on every PR and push to `main`.

## Workflow configuration

Rust retains the SPEC v2 flat Linear tracker configuration. SPEC v3's
per-kind tracker blocks, GitHub tracker, and persistent state store are
not implemented. For example:

```yaml
---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: your-project-slug
workspace:
  root: ~/symphony-workspaces
agent:
  backend: codex
codex:
  command: codex app-server
---
Work on issue {{ issue.identifier }}: {{ issue.title }}.
```

Save this as `WORKFLOW.md`, set `LINEAR_API_KEY`, and run:

```sh
./target/release/symphony doctor /path/to/WORKFLOW.md
./target/release/symphony --port 8080 /path/to/WORKFLOW.md
```

## Dependency maintenance

`vendor/liquid-core` is a checksum-verified copy of Liquid core 0.26.11
with its unmaintained `anymap2` dependency replaced by `anymap3` under the
existing import alias. See [vendor/README.md](vendor/README.md) for the
source checksum, patch, and update procedure.

## Live integration tests

Two ignored-by-default test files cover the SPEC §17.8 *Real Integration
Profile*:

```sh
# real codex on PATH (or $CODEX_BIN)
cargo test -p symphony-codex --test live_codex -- --ignored

# real Linear API
LINEAR_API_KEY=... cargo test -p symphony-tracker --test live_linear -- --ignored
```
