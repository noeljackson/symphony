# Symphony — agent guide

This file is the entry point for any AI coding agent (Claude Code, Codex,
Cursor, etc.) working in this repository. `CLAUDE.md` is a symlink to this
file so the same guidance applies regardless of which harness is used.

## What this repo is

Symphony polls Linear, GitHub Issues, or Forgejo Issues, creates per-issue
workspaces, and runs a coding
agent backend against each issue. The contract lives in [`SPEC.md`](SPEC.md).
The only shipped implementation is [`rust/`](rust/), targeting SPEC v2
with the `codex` and `claude_code` backends, plus the SPEC v3 GitHub and
Forgejo tracker contracts. SPEC v3 persistent storage remains unimplemented.
Keep that distinction explicit in documentation.

## Spec-first

**Behavior changes in this repo land in `SPEC.md` first**, then in the
implementations. Concretely:

1. If your change adds, removes, or alters orchestrator-visible behavior
   (a config field, an HTTP endpoint, a runtime event, a backend
   contract), edit `SPEC.md` first and open that as a separate PR.
2. Once the spec PR is reviewed and merged, follow up with the
   implementation PR in `rust/`. Rust currently targets SPEC v2;
   implement newer SPEC contracts only intentionally.
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
  `.github/workflows/pr-description-lint.yml` (Python template validation).
  All checks must be green before merge.
- **PR validation**: `python3 .github/scripts/check_pr_body.py --file <body.md>`.
- **Roadmap**: track in-flight UX work in
  [`docs/TODO.md`](docs/TODO.md). GitHub Issues are disabled for this
  repo; the markdown checklist is the project's tracker.

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

The Rust "Real Integration Profile" tests require explicit opt-ins
matching SPEC §17.8. Report ignored tests as skipped, never as passed.
Opted-in tests fail clearly when a required credential or binary is missing.

```sh
cd rust
cargo test -p symphony-codex --test live_codex -- --ignored
cargo test -p symphony-tracker --test live_linear -- --ignored
cargo test -p symphony-tracker --test live_repository -- --ignored
```

`live_codex_turn_smoke` additionally requires
`SYMPHONY_E2E_REAL_CODEX_FULL=1`. Linear tests require `LINEAR_API_KEY`;
candidate fetch also requires `LINEAR_PROJECT_SLUG`. Repository smoke tests
require the corresponding `GITHUB_*` or `FORGEJO_*` credentials documented
in [`rust/README.md`](rust/README.md).

## Dependency maintenance

`rust/vendor/liquid-core` replaces Liquid's unmaintained `anymap2`
dependency with `anymap3`. Preserve upstream source and license files;
see [`rust/vendor/README.md`](rust/vendor/README.md) before updating it.
The Rust lockfile is the dependency authority. Run the strict supply-chain
source gate after dependency updates, in addition to the Rust checks.

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
