# Symphony (Rust)

Rust port of Symphony, the workflow-driven coding-agent orchestrator described
in [`../SPEC.md`](../SPEC.md). The Elixir implementation in `../elixir` is
kept as prior art and reference.

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
cargo build --release
cargo test
```

CI: `.github/workflows/rust.yml` runs `cargo fmt --check`, `cargo clippy
--workspace --all-targets --locked -- -D warnings`, and `cargo test
--workspace --locked` on every PR and push to `main`.

## Live integration tests

Two ignored-by-default test files cover the SPEC §17.8 *Real Integration
Profile*:

```sh
# real codex on PATH (or $CODEX_BIN)
cargo test -p symphony-codex --test live_codex -- --ignored

# real Linear API
LINEAR_API_KEY=... cargo test -p symphony-tracker --test live_linear -- --ignored
```
