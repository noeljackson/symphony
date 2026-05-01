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

- [ ] **`symphony doctor` first-run CLI** — SPEC §18.2
  - Runs `validate_for_dispatch()` plus environment checks
  - Checklist items (each pass/fail with one-line explanation):
    - Workflow file loadable + parses
    - Tracker auth reachable (`fetch_candidate_issues` smoke against `tracker.endpoint`)
    - Agent backend prerequisite present (`codex` or `claude` on PATH for stdio backends; `api_key` set + endpoint reachable for HTTP backends)
    - Workspace root writable
    - Hook scripts parse (`bash -n`)
  - Exit `0` on full green, `1` on any failure
  - Tests: smoke test that runs against a known-bad workflow and asserts the failing checks report

### P1 — operator control surface

- [ ] **`POST /api/v1/<id>/retry`** — SPEC §13.7.3
  - Force-schedule a retry for an issue currently tracked by the orchestrator
  - `202 Accepted` with `{queued: true, issue_identifier, attempt}`
  - `404` with the standard error envelope when unknown
  - Tests: dashboard integration test that triggers retry, asserts `RetryEntry` appears in the snapshot

- [ ] **`GET /api/v1/<id>/workspace`** — SPEC §13.7.3
  - Read-only directory listing
  - Truncate large listings, validate path-traversal queries (no `..`)
  - Don't auto-serve binaries
  - Tests: integration test for a workspace with a few files, plus an attempted `..` traversal that returns 400

- [ ] **`GET /api/v1/<id>/workspace/<file>`** — SPEC §13.7.3
  - Single-file fetch under the per-issue workspace
  - Enforces §9.5 root-prefix containment
  - Caps response size; sets `text/plain; charset=utf-8` for text, `application/octet-stream` otherwise

### P2 — operator quality of life

- [ ] **`symphony logs <identifier>`** — SPEC §18.2
  - Tails the agent-session logs surfaced by the snapshot's `agent_session_logs` array
  - No need for the operator to know the on-disk layout
  - Tests: round-trip test that boots a fake-agent run, captures logs, then tails them via the CLI

- [ ] **Per-issue cost tracking + daily budget cap** — SPEC §18.2
  - `cost_usd` field on `agent_totals`
  - Optional `agent.daily_budget_usd` config field
  - Documented hard-stop / warning behavior when the cap is reached
  - Per-process / per-project / per-tracker scope explicitly documented in the implementation
  - Tests: unit test for cost extraction across each backend's usage payload, plus a budget-cap integration test

- [ ] **WORKFLOW.md JSON Schema** — SPEC §18.2
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
