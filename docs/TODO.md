# Symphony — UX roadmap

GitHub Issues are disabled for this repository, so this checklist is the
project's tracker for in-flight UX work. Each item links to its SPEC v2
section. PRs that close an item should check it off in the same change.

Items are ordered roughly by user-visible payoff. Anything labelled "P0"
should land before broader feature work; the rest are pull-as-needed.

## Priority

### P0 — operator visibility

- [x] **Live event stream (SSE)** — SPEC §13.7.4 (PR #10)
  - `GET /api/v1/events` with `text/event-stream`
  - Initial `event: snapshot` matches `GET /api/v1/state`
  - Per-event `event:` = `RuntimeEvent.event`, `data:` = `{issue_identifier, session_id, timestamp, …}`
  - Drop-on-backpressure with explicit `event: lagged` so clients can re-snapshot
  - Dashboard at `/` consumes the stream and updates without page reloads
  - Tests: integration test that connects via reqwest, asserts initial snapshot + at least one notification during a worker run
  - Crates touched: `symphony-orchestrator` (broadcast channel from the actor's `CodexUpdate` flow), `symphony-http` (`axum::response::sse`)

### P1 — first-run friction

- [x] **`symphony doctor` first-run CLI** — SPEC §18.2 (PR #11)
  - Runs `validate_for_dispatch()` plus environment checks
  - Checklist items (each pass/fail with one-line explanation):
    - Workflow file loadable + parses
    - Tracker auth reachable (`fetch_candidate_issues` smoke against `tracker.endpoint`, 10s timeout)
    - Agent backend prerequisite present (`codex` or `claude` on PATH for stdio backends; HTTP backends rejected as TBD until those crates land)
    - Workspace root writable
    - Hook scripts parse (`bash -n`)
  - Exit `0` on full green, `1` on any failure
  - Tests: 4 unit tests in `doctor::tests` + 2 binary smoke tests in `cli_smoke.rs`

### P1 — operator control surface

- [x] **`POST /api/v1/<id>/retry`** — SPEC §13.7.3 (PR #12)
  - Force-schedule a retry for an issue currently tracked by the orchestrator
  - `202 Accepted` with `{queued, issue_identifier, attempt}` for retries; `{queued: false, status: "already_running"}` if the worker is already active
  - `404` with the standard error envelope when unknown
  - Tests: integration test that triggers retry against a running worker, asserts the `already_running` shape

- [x] **`GET /api/v1/<id>/workspace`** — SPEC §13.7.3 (PR #12)
  - Read-only recursive directory listing
  - Truncates at 1000 entries (`WORKSPACE_LISTING_LIMIT`)
  - Returns 503 when server was started without a workspace root, 404 when the workspace dir doesn't exist
  - Tests: lists files when populated, 404 on missing, 503 when disabled

- [x] **`GET /api/v1/<id>/workspace/<file>`** — SPEC §13.7.3 (PR #12)
  - Single-file fetch under the per-issue workspace
  - Enforces §9.5 root-prefix containment via canonicalized prefix check
  - Caps response size at 2 MiB (`WORKSPACE_FILE_BYTE_LIMIT`); 413 above that
  - Content-type: `text/plain; charset=utf-8` for text, `application/octet-stream` otherwise
  - Tests: text file round-trip + `..%2F` traversal rejected with 400

### P2 — operator quality of life

- [ ] **`symphony logs <identifier>`** — SPEC §18.2
  - Tails the agent-session logs surfaced by the snapshot's `agent_session_logs` array
  - No need for the operator to know the on-disk layout
  - Tests: round-trip test that boots a fake-agent run, captures logs, then tails them via the CLI

- [x] **Per-issue cost tracking + daily budget cap** — SPEC §13.5 / §5.3.5
      (spec PR #14, Rust impl PR #15)
  - `cost_usd` + `cost_usd_today` (Option<f64>) on `agent_totals`,
    surfaced in `/api/v1/state`
  - Optional `agent.daily_budget_usd` config field with positive-number
    validation
  - Hard-stop on dispatch when `cost_usd_today >= daily_budget_usd`;
    already-running workers continue. Lazy UTC-day rollover at tick time.
  - Pluggable `PriceTable` keyed by `(backend, model)`; built-in table is
    empty today (codex / claude_code are subscription-priced) but
    `openai_compat` / `anthropic_messages` will populate it when those
    crates land. `null` cost disables the cap per SPEC §13.5.
  - Per-process scope; multi-process aggregation is OUT OF SCOPE per
    SPEC §13.5.
  - Tests: state-helper unit tests (rollover, cap threshold, no-pricing
    inertness), pricing-table unit tests, config-parsing tests, and
    integration tests gating dispatch on the cap.

- [x] **WORKFLOW.md JSON Schema** — SPEC §18.2 (PR #13)
  - Published JSON Schema for the §5.3 front-matter schema
  - Exposed as `docs/workflow.schema.json` so editors (VS Code, Zed) can fetch it
  - Tests: schema-validation test against every fixture WORKFLOW.md in the repo

- [ ] **Multi-workflow process mode** — SPEC §18.2
  - One `symphony` process drives multiple `WORKFLOW.md` files concurrently
  - Shared HTTP server + lifecycle, isolated orchestrator state per workflow
  - Useful when one operator runs Symphony against several Linear projects on one host
  - Tests: integration test booting two memory trackers under one process, asserting each workflow's snapshot is independent

## Not on this list (out of scope)

- WebSocket transport for the event stream (SSE is enough for now)
- Authentication on the dashboard (assumes loopback / private network)
- Rich workspace editor in the browser (read-only browse only)
- Pluggable issue tracker adapters beyond Linear (tracked separately in SPEC §18.2's existing TODOs)

## Conventions

- Each item gets one PR. Branch name `claude/<short>` per [`AGENTS.md`](../AGENTS.md).
- Spec-first: if the implementation reveals a spec ambiguity, open the spec
  clarification PR first and reference it from the implementation PR.
- Update this file in the same PR that closes the item — check the box and
  link the merge SHA.
