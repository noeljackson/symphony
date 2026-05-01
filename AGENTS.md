# Symphony — agent guide

This file is the entry point for any AI coding agent (Claude Code, Codex,
Cursor, etc.) working in this repository. `CLAUDE.md` is a symlink to this
file so the same guidance applies regardless of which harness is used.

## What this repo is

Symphony is an orchestrator that polls an issue tracker (Linear today),
creates per-issue workspaces, and runs a coding agent backend against
each issue. The contract lives in [`SPEC.md`](SPEC.md). Two reference
implementations live alongside it:

| Tree | Status |
|---|---|
| [`elixir/`](elixir/) | Original reference. Single backend (`codex`). Phoenix dashboard, full feature tour. |
| [`rust/`](rust/) | Newer reference. Tracks SPEC v2 directly. Two backends today (`codex`, `claude_code`); `openai_compat` and `anthropic_messages` TBD. |

## Spec-first

**Behavior changes in this repo land in `SPEC.md` first**, then in the
implementations. Concretely:

1. If your change adds, removes, or alters orchestrator-visible behavior
   (a config field, an HTTP endpoint, a runtime event, a backend
   contract), edit `SPEC.md` first and open that as a separate PR.
2. Once the spec PR is reviewed and merged, follow up with one
   implementation PR per tree (`elixir/`, `rust/`).
3. Pure bug fixes / refactors / test additions that don't change spec
   behavior can skip step 1.

Why this order: the spec is the human-readable contract. If we let
implementations drift first, the references diverge and the next port
becomes archaeology. Past experience in this repo (PRs #4 and #5)
showed that v2 field renames done implementation-first hid CI
regressions and caused several follow-up cleanup PRs.

If a spec PR is already pending, mention it in the implementation PR
and defer until merge.

## Repo conventions

- **Branch naming**: `claude/<short-description>` (e.g.
  `claude/spec-multi-backend`, `claude/rust-claude-code-backend`). The
  CI Git Development Branch instructions enforce that AI-driven work
  goes on a `claude/*` branch and never directly to `main`.
- **PR template**: see [`.github/pull_request_template.md`](.github/pull_request_template.md).
  The `validate-pr-description` workflow rejects PRs that don't include
  the `#### Context / TL;DR / Summary / Alternatives / Test Plan` sections.
- **CI**: `.github/workflows/rust.yml` (fmt + clippy + test) and
  `.github/workflows/make-all.yml` (Elixir setup + build + fmt-check
  + lint + test + dialyzer). All checks must be green before merge.
- **Roadmap**: track in-flight UX work in
  [`docs/TODO.md`](docs/TODO.md). GitHub Issues are disabled for this
  repo; the markdown checklist is the project's tracker.

## Working in `elixir/`

See [`elixir/AGENTS.md`](elixir/AGENTS.md) for tree-specific rules
(specs.check requirements, mix lint config, workspace safety
invariants, etc.). The high-level workflow is:

```sh
cd elixir
make setup    # mix deps.get + compile
make all      # fmt-check + lint + coverage + dialyzer
```

mix format runs against the **CI-pinned Elixir 1.19**; running mix
format locally with an older Elixir (1.14, etc.) will silently revert
1.19-specific normalization (e.g. `(() -> T)` → `(-> T)`) and CI will
fail. If you don't have 1.19 locally, push and let the CI's
diagnostics PR-comment surface the diff.

## Working in `rust/`

The Rust workspace lives in [`rust/`](rust/). High-level workflow:

```sh
cd rust
cargo fmt --all --check
cargo clippy --workspace --all-targets --locked -- -D warnings
cargo test --workspace --locked
```

Phase boundaries from the original port live in
[`rust/README.md`](rust/README.md). Backend status:

- `codex`: implemented (`symphony-codex`)
- `claude_code`: implemented (`symphony-claude-code`)
- `openai_compat`: TBD
- `anthropic_messages`: TBD

`RealWorker` dispatches on `cfg.agent.backend`; per-backend session
methods (`run_codex_session`, `run_claude_code_session`) share the
workspace + hook lifecycle but inline their own turn loops on purpose
— the closure-based shared loop fights the borrow checker for
`&mut self` clients.

## Live integration tests

Both trees keep "Real Integration Profile" tests behind explicit
opt-ins matching SPEC §17.8. Skipped tests must be **reported as
skipped, not silently treated as passed** — local panics with a clear
message when the prerequisite credential / binary is missing.

```sh
# Rust
cargo test -p symphony-codex --test live_codex -- --ignored
cargo test -p symphony-tracker --test live_linear -- --ignored

# Elixir
SYMPHONY_RUN_LIVE_E2E=1 mix test test/symphony_elixir/live_e2e_test.exs
```

## Safety rails

- Never run a coding agent directly in this source tree. Workspaces
  MUST stay under the configured `workspace.root` (SPEC §9.5).
- Don't bypass git hooks (`--no-verify`) without explicit instruction.
- Don't push to `main` directly. Always go through a PR + CI.
- Hooks (`after_create` / `before_run` / `after_run` / `before_remove`)
  are arbitrary shell scripts read from `WORKFLOW.md` — treat them as
  fully-trusted configuration but always run with a hook timeout
  (`hooks.timeout_ms`).
- Secret handling: `$VAR` indirection in workflow config is the
  preferred way to surface credentials. Never log secret values.

## Where to ask

- Spec ambiguities → open a spec PR with the proposed clarification
  and explicit "this is a spec-clarification PR; no behavior change"
  in the body.
- Implementation choices that don't touch the spec → just open the
  implementation PR with rationale in the description.
