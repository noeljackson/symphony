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
| `symphony-tracker` | `Tracker` trait + Linear, GitHub, Forgejo readers + `linear_graphql` tool |
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

Rust supports Linear, GitHub Issues, and Forgejo Issues. Use the SPEC v3
per-kind configuration blocks. Existing SPEC v2 flat Linear workflows
remain supported; a top-level `linear:` block takes full precedence.
The SPEC v3 persistent state store remains unimplemented. For example:

```yaml
---
tracker:
  kind: linear
linear:
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

## Repository issue trackers

For GitHub Issues, select `github` and configure one repository:

```yaml
tracker:
  kind: github
  active_states: [ready, in-progress]
  terminal_states: [done, closed]
github:
  owner: your-organization
  repo: your-repository
  api_token: $GITHUB_TOKEN
  label_priority_map:
    urgent: 0
    normal: 1
  # assignee: your-login
```

For Forgejo, select `forgejo` and provide the full instance API base:

```yaml
tracker:
  kind: forgejo
  active_states: [ready, in-progress]
  terminal_states: [done, closed]
forgejo:
  endpoint: https://forge.example/api/v1
  owner: your-organization
  repo: your-repository
  api_token: $FORGEJO_TOKEN
  label_priority_map:
    urgent: 0
  # assignee: your-login
```

Combine either example with the `workspace`, `agent`, and prompt sections
of the complete workflow above. Run `symphony doctor` before starting it.
Use a GitHub token with the permissions required by SPEC §5.3.1.B, or a
Forgejo token permitted to read repository issues. Agent tools that write
issues may need additional permissions; Symphony's adapters only read.

GitHub App credentials take precedence over `api_token`. Configure all
three fields together; `private_key` contains PEM material, not a filename:

```yaml
github:
  owner: your-organization
  repo: your-repository
  app_id: '123'
  app_installation_id: '456'
  private_key: $GITHUB_APP_PRIVATE_KEY
```

The reader signs App JWTs locally, exchanges them for repository-scoped
installation tokens with issue-read permission, caches those tokens, and
refreshes before expiry or once after an authentication failure. GitHub
Enterprise API prefixes are preserved, for example
`https://github.example/api/v3`. Forgejo deployment prefixes are also
preserved, for example `https://forge.example/git/api/v1`.

Both adapters match state labels case-insensitively. Terminal labels take
precedence over active labels. Closed issues always map to a terminal
state, preferring configured `closed`, otherwise the first terminal state.
A terminal-state list must contain at least one state. Unlabelled open
issues are eligible only with `open` explicitly configured as active.
The lowest matching label priority wins; the default is `1`. An assignee
filter applies to candidate dispatch, while cleanup and reconciliation
still see issues whose assignee changed. Pull requests are excluded.
Descriptions and timestamps are retained; native blocker dependencies
are currently not mapped (`blocked_by` is empty).

Issue IDs include the tracker instance, repository, and issue number so
reconciliation works after a restart without an in-memory lookup table.
Only Linear advertises the `linear_graphql` agent tool. Configure GitHub
or Forgejo agent tools in the workflow's prompt or agent environment.

Restart after changing tracker configuration, including repository,
credentials, state labels, priorities, or assignee. The current Rust
tracker clients capture these settings at startup. Polling and concurrency
continue to reload through the existing watcher.

## Dependency maintenance

`vendor/liquid-core` is a checksum-verified copy of Liquid core 0.26.11
with its unmaintained `anymap2` dependency replaced by `anymap3` under the
existing import alias. See [vendor/README.md](vendor/README.md) for the
source checksum, patch, and update procedure.

## Live integration tests

Ignored-by-default test files cover the SPEC §17.8 *Real Integration
Profile*:

```sh
# real codex on PATH (or $CODEX_BIN)
cargo test -p symphony-codex --test live_codex -- --ignored

# real Linear API
LINEAR_API_KEY=... cargo test -p symphony-tracker --test live_linear -- --ignored

# real GitHub API: set GITHUB_TOKEN, GITHUB_OWNER, GITHUB_REPO
cargo test -p symphony-tracker --test live_repository live_github -- --ignored

# real Forgejo API: set FORGEJO_TOKEN, FORGEJO_OWNER, FORGEJO_REPO, FORGEJO_API_URL
cargo test -p symphony-tracker --test live_repository live_forgejo -- --ignored
```
