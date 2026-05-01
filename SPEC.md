# Symphony Service Specification

Status: Draft v2 (language-agnostic, multi-backend)

Purpose: Define a service that orchestrates coding agents to get project work done.

v2 generalizes the agent-runner contract so any compatible backend (Codex
app-server, Claude Code, OpenAI-compatible HTTP APIs such as Kimi K2 and GLM,
the Anthropic Messages API, and others) can be plugged in. Backends are
selected per-workflow via `agent.backend`. v2 is a clean break from v1's
Codex-only assumptions: runtime fields named `codex_*` are renamed to
`agent_*`, and the §10 protocol section is restructured as a generic contract
plus per-backend subsections.

## Normative Language

The key words `MUST`, `MUST NOT`, `REQUIRED`, `SHOULD`, `SHOULD NOT`, `RECOMMENDED`, `MAY`, and
`OPTIONAL` in this document are to be interpreted as described in RFC 2119.

`Implementation-defined` means the behavior is part of the implementation contract, but this
specification does not prescribe one universal policy. Implementations MUST document the selected
behavior.

## 1. Problem Statement

Symphony is a long-running automation service that continuously reads work from an issue tracker
(Linear in this specification version), creates an isolated workspace for each issue, and runs a
coding agent session for that issue inside the workspace.

The service solves four operational problems:

- It turns issue execution into a repeatable daemon workflow instead of manual scripts.
- It isolates agent execution in per-issue workspaces so agent commands run only inside per-issue
  workspace directories.
- It keeps the workflow policy in-repo (`WORKFLOW.md`) so teams version the agent prompt and runtime
  settings with their code.
- It provides enough observability to operate and debug multiple concurrent agent runs.

Implementations are expected to document their trust and safety posture explicitly. This
specification does not require a single approval, sandbox, or operator-confirmation policy; some
implementations target trusted environments with a high-trust configuration, while others require
stricter approvals or sandboxing.

Important boundary:

- Symphony is a scheduler/runner and tracker reader.
- Ticket writes (state transitions, comments, PR links) are typically performed by the coding agent
  using tools available in the workflow/runtime environment.
- A successful run can end at a workflow-defined handoff state (for example `Human Review`), not
  necessarily `Done`.

## 2. Goals and Non-Goals

### 2.1 Goals

- Poll the issue tracker on a fixed cadence and dispatch work with bounded concurrency.
- Maintain a single authoritative orchestrator state for dispatch, retries, and reconciliation.
- Create deterministic per-issue workspaces and preserve them across runs.
- Stop active runs when issue state changes make them ineligible.
- Recover from transient failures with exponential backoff.
- Load runtime behavior from a repository-owned `WORKFLOW.md` contract.
- Expose operator-visible observability (at minimum structured logs).
- Support tracker/filesystem-driven restart recovery without requiring a persistent database; exact
  in-memory scheduler state is not restored.

### 2.2 Non-Goals

- Rich web UI or multi-tenant control plane.
- Prescribing a specific dashboard or terminal UI implementation.
- General-purpose workflow engine or distributed job scheduler.
- Built-in business logic for how to edit tickets, PRs, or comments. (That logic lives in the
  workflow prompt and agent tooling.)
- Mandating strong sandbox controls beyond what the coding agent and host OS provide.
- Mandating a single default approval, sandbox, or operator-confirmation posture for all
  implementations.

## 3. System Overview

### 3.1 Main Components

1. `Workflow Loader`
   - Reads `WORKFLOW.md`.
   - Parses YAML front matter and prompt body.
   - Returns `{config, prompt_template}`.

2. `Config Layer`
   - Exposes typed getters for workflow config values.
   - Applies defaults and environment variable indirection.
   - Performs validation used by the orchestrator before dispatch.

3. `Issue Tracker Client`
   - Fetches candidate issues in active states.
   - Fetches current states for specific issue IDs (reconciliation).
   - Fetches terminal-state issues during startup cleanup.
   - Normalizes tracker payloads into a stable issue model.

4. `Orchestrator`
   - Owns the poll tick.
   - Owns the in-memory runtime state.
   - Decides which issues to dispatch, retry, stop, or release.
   - Tracks session metrics and retry queue state.

5. `Workspace Manager`
   - Maps issue identifiers to workspace paths.
   - Ensures per-issue workspace directories exist.
   - Runs workspace lifecycle hooks.
   - Cleans workspaces for terminal issues.

6. `Agent Runner`
   - Creates workspace.
   - Builds prompt from issue + workflow template.
   - Launches the configured agent backend client (subprocess for Codex /
     Claude Code; in-process HTTP loop for OpenAI-compat / Anthropic
     Messages — see §10).
   - Streams agent updates back to the orchestrator.

7. `Status Surface` (OPTIONAL)
   - Presents human-readable runtime status (for example terminal output, dashboard, or other
     operator-facing view).

8. `Logging`
   - Emits structured runtime logs to one or more configured sinks.

### 3.2 Abstraction Levels

Symphony is easiest to port when kept in these layers:

1. `Policy Layer` (repo-defined)
   - `WORKFLOW.md` prompt body.
   - Team-specific rules for ticket handling, validation, and handoff.

2. `Configuration Layer` (typed getters)
   - Parses front matter into typed runtime settings.
   - Handles defaults, environment tokens, and path normalization.

3. `Coordination Layer` (orchestrator)
   - Polling loop, issue eligibility, concurrency, retries, reconciliation.

4. `Execution Layer` (workspace + agent backend client)
   - Filesystem lifecycle, workspace preparation, backend protocol.

5. `Integration Layer` (Linear adapter)
   - API calls and normalization for tracker data.

6. `Observability Layer` (logs + OPTIONAL status surface)
   - Operator visibility into orchestrator and agent behavior.

### 3.3 External Dependencies

- Issue tracker API (Linear for `tracker.kind: linear` in this specification version).
- Local filesystem for workspaces and logs.
- OPTIONAL workspace population tooling (for example Git CLI, if used).
- A compatible agent runner backend (subprocess CLI such as `codex app-server`
  or `claude`, or an HTTP-based LLM endpoint such as Anthropic Messages,
  OpenAI Responses, Moonshot Kimi, or Zhipu GLM). See §10 for the full list
  and per-backend contracts.
- Host environment authentication for the issue tracker and the selected
  agent backend.

## 4. Core Domain Model

### 4.1 Entities

#### 4.1.1 Issue

Normalized issue record used by orchestration, prompt rendering, and observability output.

Fields:

- `id` (string)
  - Stable tracker-internal ID.
- `identifier` (string)
  - Human-readable ticket key (example: `ABC-123`).
- `title` (string)
- `description` (string or null)
- `priority` (integer or null)
  - Lower numbers are higher priority in dispatch sorting.
- `state` (string)
  - Current tracker state name.
- `branch_name` (string or null)
  - Tracker-provided branch metadata if available.
- `url` (string or null)
- `labels` (list of strings)
  - Normalized to lowercase.
- `blocked_by` (list of blocker refs)
  - Each blocker ref contains:
    - `id` (string or null)
    - `identifier` (string or null)
    - `state` (string or null)
- `created_at` (timestamp or null)
- `updated_at` (timestamp or null)

#### 4.1.2 Workflow Definition

Parsed `WORKFLOW.md` payload:

- `config` (map)
  - YAML front matter root object.
- `prompt_template` (string)
  - Markdown body after front matter, trimmed.

#### 4.1.3 Service Config (Typed View)

Typed runtime values derived from `WorkflowDefinition.config` plus environment resolution.

Examples:

- poll interval
- workspace root
- active and terminal issue states
- concurrency limits
- agent backend selector and per-backend executable / endpoint / model /
  timeout settings
- workspace hooks

#### 4.1.4 Workspace

Filesystem workspace assigned to one issue identifier.

Fields (logical):

- `path` (absolute workspace path)
- `workspace_key` (sanitized issue identifier)
- `created_now` (boolean, used to gate `after_create` hook)

#### 4.1.5 Run Attempt

One execution attempt for one issue.

Fields (logical):

- `issue_id`
- `issue_identifier`
- `attempt` (integer or null, `null` for first run, `>=1` for retries/continuation)
- `workspace_path`
- `started_at`
- `status`
- `error` (OPTIONAL)

#### 4.1.6 Live Session (Agent Session Metadata)

State tracked while an agent backend session is active. For subprocess
backends `agent_runner_pid` is the child PID; for in-process HTTP backends
it is `null`.

Fields:

- `session_id` (string, `<thread_id>-<turn_id>`)
- `thread_id` (string)
- `turn_id` (string)
- `agent_runner_pid` (string or null)
- `last_agent_event` (string/enum or null)
- `last_agent_timestamp` (timestamp or null)
- `last_agent_message` (summarized payload)
- `agent_input_tokens` (integer)
- `agent_output_tokens` (integer)
- `agent_total_tokens` (integer)
- `last_reported_input_tokens` (integer)
- `last_reported_output_tokens` (integer)
- `last_reported_total_tokens` (integer)
- `turn_count` (integer)
  - Number of agent turns started within the current worker lifetime.

#### 4.1.7 Retry Entry

Scheduled retry state for an issue.

Fields:

- `issue_id`
- `identifier` (best-effort human ID for status surfaces/logs)
- `attempt` (integer, 1-based for retry queue)
- `due_at_ms` (monotonic clock timestamp)
- `timer_handle` (runtime-specific timer reference)
- `error` (string or null)

#### 4.1.8 Orchestrator Runtime State

Single authoritative in-memory state owned by the orchestrator.

Fields:

- `poll_interval_ms` (current effective poll interval)
- `max_concurrent_agents` (current effective global concurrency limit)
- `running` (map `issue_id -> running entry`)
- `claimed` (set of issue IDs reserved/running/retrying)
- `retry_attempts` (map `issue_id -> RetryEntry`)
- `completed` (set of issue IDs; bookkeeping only, not dispatch gating)
- `agent_totals` (aggregate tokens + runtime seconds)
- `agent_rate_limits` (latest rate-limit snapshot from agent events)

### 4.2 Stable Identifiers and Normalization Rules

- `Issue ID`
  - Use for tracker lookups and internal map keys.
- `Issue Identifier`
  - Use for human-readable logs and workspace naming.
- `Workspace Key`
  - Derive from `issue.identifier` by replacing any character not in `[A-Za-z0-9._-]` with `_`.
  - Use the sanitized value for the workspace directory name.
- `Normalized Issue State`
  - Compare states after `lowercase`.
- `Session ID`
  - Compose from the backend-provided or backend-synthesized `thread_id` and
    `turn_id` as `<thread_id>-<turn_id>`. See §10.1 for synthesis rules when
    the backend has no native thread identity.

## 5. Workflow Specification (Repository Contract)

### 5.1 File Discovery and Path Resolution

Workflow file path precedence:

1. Explicit application/runtime setting (set by CLI startup path).
2. Default: `WORKFLOW.md` in the current process working directory.

Loader behavior:

- If the file cannot be read, return `missing_workflow_file` error.
- The workflow file is expected to be repository-owned and version-controlled.

### 5.2 File Format

`WORKFLOW.md` is a Markdown file with OPTIONAL YAML front matter.

Design note:

- `WORKFLOW.md` SHOULD be self-contained enough to describe and run different workflows (prompt,
  runtime settings, hooks, and tracker selection/config) without requiring out-of-band
  service-specific configuration.

Parsing rules:

- If file starts with `---`, parse lines until the next `---` as YAML front matter.
- Remaining lines become the prompt body.
- If front matter is absent, treat the entire file as prompt body and use an empty config map.
- YAML front matter MUST decode to a map/object; non-map YAML is an error.
- Prompt body is trimmed before use.

Returned workflow object:

- `config`: front matter root object (not nested under a `config` key).
- `prompt_template`: trimmed Markdown body.

### 5.3 Front Matter Schema

Top-level keys:

- `tracker`
- `polling`
- `workspace`
- `hooks`
- `agent`
- One backend-specific configuration block matching `agent.backend`
  (`codex`, `claude_code`, `openai_compat`, or `anthropic_messages`).

Unknown keys SHOULD be ignored for forward compatibility.

Note:

- The workflow front matter is extensible. Extensions MAY define additional top-level keys without
  changing the core schema above.
- Extensions SHOULD document their field schema, defaults, validation rules, and whether changes
  apply dynamically or require restart.

#### 5.3.1 `tracker` (object)

Fields:

- `kind` (string)
  - REQUIRED for dispatch.
  - Current supported value: `linear`
- `endpoint` (string)
  - Default for `tracker.kind == "linear"`: `https://api.linear.app/graphql`
- `api_key` (string)
  - MAY be a literal token or `$VAR_NAME`.
  - Canonical environment variable for `tracker.kind == "linear"`: `LINEAR_API_KEY`.
  - If `$VAR_NAME` resolves to an empty string, treat the key as missing.
- `project_slug` (string)
  - REQUIRED for dispatch when `tracker.kind == "linear"`.
- `active_states` (list of strings)
  - Default: `Todo`, `In Progress`
- `terminal_states` (list of strings)
  - Default: `Closed`, `Cancelled`, `Canceled`, `Duplicate`, `Done`

#### 5.3.2 `polling` (object)

Fields:

- `interval_ms` (integer)
  - Default: `30000`
  - Changes SHOULD be re-applied at runtime and affect future tick scheduling without restart.

#### 5.3.3 `workspace` (object)

Fields:

- `root` (path string or `$VAR`)
  - Default: `<system-temp>/symphony_workspaces`
  - `~` is expanded.
  - Relative paths are resolved relative to the directory containing `WORKFLOW.md`.
  - The effective workspace root is normalized to an absolute path before use.

#### 5.3.4 `hooks` (object)

Fields:

- `after_create` (multiline shell script string, OPTIONAL)
  - Runs only when a workspace directory is newly created.
  - Failure aborts workspace creation.
- `before_run` (multiline shell script string, OPTIONAL)
  - Runs before each agent attempt after workspace preparation and before launching the coding
    agent.
  - Failure aborts the current attempt.
- `after_run` (multiline shell script string, OPTIONAL)
  - Runs after each agent attempt (success, failure, timeout, or cancellation) once the workspace
    exists.
  - Failure is logged but ignored.
- `before_remove` (multiline shell script string, OPTIONAL)
  - Runs before workspace deletion if the directory exists.
  - Failure is logged but ignored; cleanup still proceeds.
- `timeout_ms` (integer, OPTIONAL)
  - Default: `60000`
  - Applies to all workspace hooks.
  - Invalid values fail configuration validation.
  - Changes SHOULD be re-applied at runtime for future hook executions.

#### 5.3.5 `agent` (object)

Fields:

- `backend` (string)
  - REQUIRED for dispatch.
  - Selects which agent runner Symphony will launch for each issue.
  - Supported values for v2 core conformance:
    - `codex` — Codex stdio app-server (see §10.A)
    - `claude_code` — Claude Code stdio (see §10.B)
    - `openai_compat` — OpenAI-compatible HTTP endpoint, covers OpenAI,
      Moonshot Kimi K2, Zhipu GLM, DeepSeek, vLLM, and similar (see §10.C)
    - `anthropic_messages` — Anthropic Messages API (see §10.D)
  - Implementations MAY support a subset and MUST document which backends are
    available.
  - Unknown values fail dispatch preflight validation.
- `max_concurrent_agents` (integer)
  - Default: `10`
  - Changes SHOULD be re-applied at runtime and affect subsequent dispatch decisions.
- `max_turns` (positive integer)
  - Default: `20`
  - Limits the number of agent turns within one worker session.
  - Invalid values fail configuration validation.
- `max_retry_backoff_ms` (integer)
  - Default: `300000` (5 minutes)
  - Changes SHOULD be re-applied at runtime and affect future retry scheduling.
- `max_concurrent_agents_by_state` (map `state_name -> positive integer`)
  - Default: empty map.
  - State keys are normalized (`lowercase`) for lookup.
  - Invalid entries (non-positive or non-numeric) are ignored.

#### 5.3.6 Backend-specific configuration blocks

Exactly one backend block SHOULD be populated, matching the value of
`agent.backend`. Other blocks MAY be present but are ignored. Implementations
MUST document which backends they support and which fields are honored.

The shared timeout fields below have the same meaning across every backend:

- `turn_timeout_ms` (integer)
  - Default: `3600000` (1 hour)
  - Total wall-clock budget for one agent turn, including streaming.
- `read_timeout_ms` (integer)
  - Default: `5000`
  - Per-message / per-request response timeout during startup and sync calls.
- `stall_timeout_ms` (integer)
  - Default: `300000` (5 minutes)
  - Orchestrator-enforced inactivity window between agent updates. If `<= 0`,
    stall detection is disabled.

##### 5.3.6.A `codex` (object)

Used when `agent.backend == "codex"`.

For Codex-owned config values such as `approval_policy`, `thread_sandbox`, and
`turn_sandbox_policy`, supported values are defined by the targeted Codex app-server version.
Implementors SHOULD treat them as pass-through Codex config values rather than relying on a
hand-maintained enum in this spec. To inspect the installed Codex schema, run
`codex app-server generate-json-schema --out <dir>` and inspect the relevant definitions referenced
by `v2/ThreadStartParams.json` and `v2/TurnStartParams.json`. Implementations MAY validate these
fields locally if they want stricter startup checks.

- `command` (string shell command)
  - Default: `codex app-server`
  - The runtime launches this command via `bash -lc` in the workspace directory.
  - The launched process MUST speak a compatible app-server protocol over stdio.
- `approval_policy` (Codex `AskForApproval` value)
  - Default: implementation-defined.
- `thread_sandbox` (Codex `SandboxMode` value)
  - Default: implementation-defined.
- `turn_sandbox_policy` (Codex `SandboxPolicy` value)
  - Default: implementation-defined.
- `turn_timeout_ms`, `read_timeout_ms`, `stall_timeout_ms` — see shared timeouts above.

##### 5.3.6.B `claude_code` (object)

Used when `agent.backend == "claude_code"`. Targets the `claude` CLI's
streaming JSON mode.

- `command` (string shell command)
  - Default: `claude --print --output-format stream-json --input-format stream-json --verbose`
  - Launched via `bash -lc` in the workspace directory.
  - The launched process MUST speak Claude Code's stream-json protocol over
    stdio.
- `permission_mode` (string, OPTIONAL)
  - Maps to Claude Code's `--permission-mode` argument
    (`default | plan | acceptEdits | bypassPermissions`). Default:
    implementation-defined; high-trust configurations typically use
    `bypassPermissions`.
- `allowed_tools` (list of strings, OPTIONAL)
  - Maps to Claude Code's `--allowed-tools`. Restricts which built-in
    Claude Code tools the session may use.
- `disallowed_tools` (list of strings, OPTIONAL)
  - Maps to Claude Code's `--disallowed-tools`.
- `model` (string, OPTIONAL)
  - Specific Claude model alias to pass via `--model`. Default: implementation-defined.
- `turn_timeout_ms`, `read_timeout_ms`, `stall_timeout_ms` — see shared timeouts above.

##### 5.3.6.C `openai_compat` (object)

Used when `agent.backend == "openai_compat"`. Drives any chat-completions
endpoint that follows the OpenAI `POST /v1/chat/completions` schema. This
covers OpenAI itself, Moonshot Kimi (`https://api.moonshot.ai/v1`),
Zhipu GLM (`https://open.bigmodel.cn/api/paas/v4`), DeepSeek, locally-hosted
vLLM / llama.cpp servers, and similar.

- `endpoint` (string, REQUIRED)
  - Base URL of the chat-completions endpoint.
  - Examples: `https://api.openai.com/v1`, `https://api.moonshot.ai/v1`,
    `https://open.bigmodel.cn/api/paas/v4`.
- `api_key` (string, REQUIRED)
  - MAY be a literal token or `$VAR_NAME`.
- `model` (string, REQUIRED)
  - Provider-specific model identifier, e.g. `gpt-4.1`, `kimi-k2`, `glm-4.6`,
    `deepseek-chat`.
- `max_tokens` (integer, OPTIONAL)
  - Per-request output cap forwarded to the provider.
- `temperature` (number, OPTIONAL)
- `extra_headers` (map of string→string, OPTIONAL)
  - Pass-through HTTP headers (for vendor-specific auth schemes or routing).
- `extra_request_fields` (map, OPTIONAL)
  - Top-level fields merged into every chat-completions request body for
    provider-specific knobs.
- `turn_timeout_ms`, `read_timeout_ms`, `stall_timeout_ms` — see shared timeouts above.

##### 5.3.6.D `anthropic_messages` (object)

Used when `agent.backend == "anthropic_messages"`. Drives the Anthropic
Messages API (`POST /v1/messages`).

- `endpoint` (string)
  - Default: `https://api.anthropic.com/v1`
- `api_key` (string, REQUIRED)
  - MAY be a literal token or `$VAR_NAME`. Canonical environment variable:
    `ANTHROPIC_API_KEY`.
- `model` (string, REQUIRED)
  - Anthropic model identifier, e.g. `claude-opus-4-7`, `claude-sonnet-4-6`,
    `claude-haiku-4-5-20251001`.
- `max_tokens` (integer)
  - Default: `8192`
- `system` (string, OPTIONAL)
  - System prompt prepended to every turn. The Markdown body of `WORKFLOW.md`
    is still the per-issue user prompt; `system` is for invariant context.
- `extra_headers` (map of string→string, OPTIONAL)
- `turn_timeout_ms`, `read_timeout_ms`, `stall_timeout_ms` — see shared timeouts above.

### 5.4 Prompt Template Contract

The Markdown body of `WORKFLOW.md` is the per-issue prompt template.

Rendering requirements:

- Use a strict template engine (Liquid-compatible semantics are sufficient).
- Unknown variables MUST fail rendering.
- Unknown filters MUST fail rendering.

Template input variables:

- `issue` (object)
  - Includes all normalized issue fields, including labels and blockers.
- `attempt` (integer or null)
  - `null`/absent on first attempt.
  - Integer on retry or continuation run.

Fallback prompt behavior:

- If the workflow prompt body is empty, the runtime MAY use a minimal default prompt
  (`You are working on an issue from Linear.`).
- Workflow file read/parse failures are configuration/validation errors and SHOULD NOT silently fall
  back to a prompt.

### 5.5 Workflow Validation and Error Surface

Error classes:

- `missing_workflow_file`
- `workflow_parse_error`
- `workflow_front_matter_not_a_map`
- `template_parse_error` (during prompt rendering)
- `template_render_error` (unknown variable/filter, invalid interpolation)

Dispatch gating behavior:

- Workflow file read/YAML errors block new dispatches until fixed.
- Template errors fail only the affected run attempt.

## 6. Configuration Specification

### 6.1 Configuration Resolution Pipeline

Configuration is resolved in this order:

1. Select the workflow file path (explicit runtime setting, otherwise cwd default).
2. Parse YAML front matter into a raw config map.
3. Apply built-in defaults for missing OPTIONAL fields.
4. Resolve `$VAR_NAME` indirection only for config values that explicitly contain `$VAR_NAME`.
5. Coerce and validate typed values.

Environment variables do not globally override YAML values. They are used only when a config value
explicitly references them.

Value coercion semantics:

- Path/command fields support:
  - `~` home expansion
  - `$VAR` expansion for env-backed path values
  - Apply expansion only to values intended to be local filesystem paths; do not rewrite URIs or
    arbitrary shell command strings.
- Relative `workspace.root` values resolve relative to the directory containing the selected
  `WORKFLOW.md`.

### 6.2 Dynamic Reload Semantics

Dynamic reload is REQUIRED:

- The software MUST detect `WORKFLOW.md` changes.
- On change, it MUST re-read and re-apply workflow config and prompt template without restart.
- The software MUST attempt to adjust live behavior to the new config (for example polling
  cadence, concurrency limits, active/terminal states, codex settings, workspace paths/hooks, and
  prompt content for future runs).
- Reloaded config applies to future dispatch, retry scheduling, reconciliation decisions, hook
  execution, and agent launches.
- Implementations are not REQUIRED to restart in-flight agent sessions automatically when config
  changes.
- Extensions that manage their own listeners/resources (for example an HTTP server port change) MAY
  require restart unless the implementation explicitly supports live rebind.
- Implementations SHOULD also re-validate/reload defensively during runtime operations (for example
  before dispatch) in case filesystem watch events are missed.
- Invalid reloads MUST NOT crash the service; keep operating with the last known good effective
  configuration and emit an operator-visible error.

### 6.3 Dispatch Preflight Validation

This validation is a scheduler preflight run before attempting to dispatch new work. It validates
the workflow/config needed to poll and launch workers, not a full audit of all possible workflow
behavior.

Startup validation:

- Validate configuration before starting the scheduling loop.
- If startup validation fails, fail startup and emit an operator-visible error.

Per-tick dispatch validation:

- Re-validate before each dispatch cycle.
- If validation fails, skip dispatch for that tick, keep reconciliation active, and emit an
  operator-visible error.

Validation checks:

- Workflow file can be loaded and parsed.
- `tracker.kind` is present and supported.
- `tracker.api_key` is present after `$` resolution.
- `tracker.project_slug` is present when REQUIRED by the selected tracker kind.
- `agent.backend` is present and refers to a backend the implementation
  supports.
- The backend-specific config block selected by `agent.backend` passes its
  own preflight (see §10.A–§10.D). For example, `agent.backend == "codex"`
  requires `codex.command` to be present and non-empty;
  `agent.backend == "openai_compat"` requires `openai_compat.endpoint`,
  `openai_compat.api_key` (after `$` resolution), and `openai_compat.model`;
  `agent.backend == "anthropic_messages"` requires
  `anthropic_messages.api_key` and `anthropic_messages.model`.

### 6.4 Core Config Fields Summary (Cheat Sheet)

This section is intentionally redundant so a coding agent can implement the config layer quickly.
Extension fields are documented in the extension section that defines them. Core conformance does
not require recognizing or validating extension fields unless that extension is implemented.

- `tracker.kind`: string, REQUIRED, currently `linear`
- `tracker.endpoint`: string, default `https://api.linear.app/graphql` when `tracker.kind=linear`
- `tracker.api_key`: string or `$VAR`, canonical env `LINEAR_API_KEY` when `tracker.kind=linear`
- `tracker.project_slug`: string, REQUIRED when `tracker.kind=linear`
- `tracker.active_states`: list of strings, default `["Todo", "In Progress"]`
- `tracker.terminal_states`: list of strings, default `["Closed", "Cancelled", "Canceled", "Duplicate", "Done"]`
- `polling.interval_ms`: integer, default `30000`
- `workspace.root`: path resolved to absolute, default `<system-temp>/symphony_workspaces`
- `hooks.after_create`: shell script or null
- `hooks.before_run`: shell script or null
- `hooks.after_run`: shell script or null
- `hooks.before_remove`: shell script or null
- `hooks.timeout_ms`: integer, default `60000`
- `agent.backend`: string, REQUIRED, one of
  `codex | claude_code | openai_compat | anthropic_messages`
- `agent.max_concurrent_agents`: integer, default `10`
- `agent.max_turns`: integer, default `20`
- `agent.max_retry_backoff_ms`: integer, default `300000` (5m)
- `agent.max_concurrent_agents_by_state`: map of positive integers, default `{}`

Backend-specific blocks (each populated only when its backend is selected):

- `codex.command`: shell command string, default `codex app-server`
- `codex.approval_policy`: Codex `AskForApproval` value, default implementation-defined
- `codex.thread_sandbox`: Codex `SandboxMode` value, default implementation-defined
- `codex.turn_sandbox_policy`: Codex `SandboxPolicy` value, default implementation-defined
- `claude_code.command`: shell command string, default
  `claude --print --output-format stream-json --input-format stream-json --verbose`
- `claude_code.permission_mode`: string, OPTIONAL
- `claude_code.allowed_tools` / `disallowed_tools`: list of strings, OPTIONAL
- `claude_code.model`: string, OPTIONAL
- `openai_compat.endpoint`: string, REQUIRED
- `openai_compat.api_key`: string or `$VAR`, REQUIRED
- `openai_compat.model`: string, REQUIRED
- `openai_compat.max_tokens` / `temperature` / `extra_headers` /
  `extra_request_fields`: OPTIONAL
- `anthropic_messages.endpoint`: string, default `https://api.anthropic.com/v1`
- `anthropic_messages.api_key`: string or `$VAR`, REQUIRED, canonical env
  `ANTHROPIC_API_KEY`
- `anthropic_messages.model`: string, REQUIRED
- `anthropic_messages.max_tokens`: integer, default `8192`
- `anthropic_messages.system` / `extra_headers`: OPTIONAL

Shared timeout fields under each backend block (same default and meaning):

- `<backend>.turn_timeout_ms`: integer, default `3600000`
- `<backend>.read_timeout_ms`: integer, default `5000`
- `<backend>.stall_timeout_ms`: integer, default `300000`

## 7. Orchestration State Machine

The orchestrator is the only component that mutates scheduling state. All worker outcomes are
reported back to it and converted into explicit state transitions.

### 7.1 Issue Orchestration States

This is not the same as tracker states (`Todo`, `In Progress`, etc.). This is the service's internal
claim state.

1. `Unclaimed`
   - Issue is not running and has no retry scheduled.

2. `Claimed`
   - Orchestrator has reserved the issue to prevent duplicate dispatch.
   - In practice, claimed issues are either `Running` or `RetryQueued`.

3. `Running`
   - Worker task exists and the issue is tracked in `running` map.

4. `RetryQueued`
   - Worker is not running, but a retry timer exists in `retry_attempts`.

5. `Released`
   - Claim removed because issue is terminal, non-active, missing, or retry path completed without
     re-dispatch.

Important nuance:

- A successful worker exit does not mean the issue is done forever.
- The worker MAY continue through multiple back-to-back agent turns before
  it exits.
- After each normal turn completion, the worker re-checks the tracker issue
  state.
- If the issue is still in an active state, the worker SHOULD start another
  turn on the same live agent session in the same workspace, up to
  `agent.max_turns`.
- The first turn SHOULD use the full rendered task prompt.
- Continuation turns SHOULD send only continuation guidance to the existing thread, not resend the
  original task prompt that is already present in thread history.
- Once the worker exits normally, the orchestrator still schedules a short continuation retry
  (about 1 second) so it can re-check whether the issue remains active and needs another worker
  session.

### 7.2 Run Attempt Lifecycle

A run attempt transitions through these phases:

1. `PreparingWorkspace`
2. `BuildingPrompt`
3. `LaunchingAgentProcess`
4. `InitializingSession`
5. `StreamingTurn`
6. `Finishing`
7. `Succeeded`
8. `Failed`
9. `TimedOut`
10. `Stalled`
11. `CanceledByReconciliation`

Distinct terminal reasons are important because retry logic and logs differ.

### 7.3 Transition Triggers

- `Poll Tick`
  - Reconcile active runs.
  - Validate config.
  - Fetch candidate issues.
  - Dispatch until slots are exhausted.

- `Worker Exit (normal)`
  - Remove running entry.
  - Update aggregate runtime totals.
  - Schedule continuation retry (attempt `1`) after the worker exhausts or finishes its in-process
    turn loop.

- `Worker Exit (abnormal)`
  - Remove running entry.
  - Update aggregate runtime totals.
  - Schedule exponential-backoff retry.

- `Agent Update Event`
  - Update live session fields, token counters, and rate limits.

- `Retry Timer Fired`
  - Re-fetch active candidates and attempt re-dispatch, or release claim if no longer eligible.

- `Reconciliation State Refresh`
  - Stop runs whose issue states are terminal or no longer active.

- `Stall Timeout`
  - Kill worker and schedule retry.

### 7.4 Idempotency and Recovery Rules

- The orchestrator serializes state mutations through one authority to avoid duplicate dispatch.
- `claimed` and `running` checks are REQUIRED before launching any worker.
- Reconciliation runs before dispatch on every tick.
- Restart recovery is tracker-driven and filesystem-driven (without a durable orchestrator DB).
- Startup terminal cleanup removes stale workspaces for issues already in terminal states.

## 8. Polling, Scheduling, and Reconciliation

### 8.1 Poll Loop

At startup, the service validates config, performs startup cleanup, schedules an immediate tick, and
then repeats every `polling.interval_ms`.

The effective poll interval SHOULD be updated when workflow config changes are re-applied.

Tick sequence:

1. Reconcile running issues.
2. Run dispatch preflight validation.
3. Fetch candidate issues from tracker using active states.
4. Sort issues by dispatch priority.
5. Dispatch eligible issues while slots remain.
6. Notify observability/status consumers of state changes.

If per-tick validation fails, dispatch is skipped for that tick, but reconciliation still happens
first.

### 8.2 Candidate Selection Rules

An issue is dispatch-eligible only if all are true:

- It has `id`, `identifier`, `title`, and `state`.
- Its state is in `active_states` and not in `terminal_states`.
- It is not already in `running`.
- It is not already in `claimed`.
- Global concurrency slots are available.
- Per-state concurrency slots are available.
- Blocker rule for `Todo` state passes:
  - If the issue state is `Todo`, do not dispatch when any blocker is non-terminal.

Sorting order (stable intent):

1. `priority` ascending (1..4 are preferred; null/unknown sorts last)
2. `created_at` oldest first
3. `identifier` lexicographic tie-breaker

### 8.3 Concurrency Control

Global limit:

- `available_slots = max(max_concurrent_agents - running_count, 0)`

Per-state limit:

- `max_concurrent_agents_by_state[state]` if present (state key normalized)
- otherwise fallback to global limit

The runtime counts issues by their current tracked state in the `running` map.

### 8.4 Retry and Backoff

Retry entry creation:

- Cancel any existing retry timer for the same issue.
- Store `attempt`, `identifier`, `error`, `due_at_ms`, and new timer handle.

Backoff formula:

- Normal continuation retries after a clean worker exit use a short fixed delay of `1000` ms.
- Failure-driven retries use `delay = min(10000 * 2^(attempt - 1), agent.max_retry_backoff_ms)`.
- Power is capped by the configured max retry backoff (default `300000` / 5m).

Retry handling behavior:

1. Fetch active candidate issues (not all issues).
2. Find the specific issue by `issue_id`.
3. If not found, release claim.
4. If found and still candidate-eligible:
   - Dispatch if slots are available.
   - Otherwise requeue with error `no available orchestrator slots`.
5. If found but no longer active, release claim.

Note:

- Terminal-state workspace cleanup is handled by startup cleanup and active-run reconciliation
  (including terminal transitions for currently running issues).
- Retry handling mainly operates on active candidates and releases claims when the issue is absent,
  rather than performing terminal cleanup itself.

### 8.5 Active Run Reconciliation

Reconciliation runs every tick and has two parts.

Part A: Stall detection

- For each running issue, compute `elapsed_ms` since:
  - `last_agent_timestamp` if any event has been seen, else
  - `started_at`
- If `elapsed_ms > <backend>.stall_timeout_ms` (where `<backend>` is the value
  of `agent.backend`), terminate the worker and queue a retry.
- If `stall_timeout_ms <= 0`, skip stall detection entirely.

Part B: Tracker state refresh

- Fetch current issue states for all running issue IDs.
- For each running issue:
  - If tracker state is terminal: terminate worker and clean workspace.
  - If tracker state is still active: update the in-memory issue snapshot.
  - If tracker state is neither active nor terminal: terminate worker without workspace cleanup.
- If state refresh fails, keep workers running and try again on the next tick.

### 8.6 Startup Terminal Workspace Cleanup

When the service starts:

1. Query tracker for issues in terminal states.
2. For each returned issue identifier, remove the corresponding workspace directory.
3. If the terminal-issues fetch fails, log a warning and continue startup.

This prevents stale terminal workspaces from accumulating after restarts.

## 9. Workspace Management and Safety

### 9.1 Workspace Layout

Workspace root:

- `workspace.root` (normalized absolute path)

Per-issue workspace path:

- `<workspace.root>/<sanitized_issue_identifier>`

Workspace persistence:

- Workspaces are reused across runs for the same issue.
- Successful runs do not auto-delete workspaces.

### 9.2 Workspace Creation and Reuse

Input: `issue.identifier`

Algorithm summary:

1. Sanitize identifier to `workspace_key`.
2. Compute workspace path under workspace root.
3. Ensure the workspace path exists as a directory.
4. Mark `created_now=true` only if the directory was created during this call; otherwise
   `created_now=false`.
5. If `created_now=true`, run `after_create` hook if configured.

Notes:

- This section does not assume any specific repository/VCS workflow.
- Workspace preparation beyond directory creation (for example dependency bootstrap, checkout/sync,
  code generation) is implementation-defined and is typically handled via hooks.

### 9.3 OPTIONAL Workspace Population (Implementation-Defined)

The spec does not require any built-in VCS or repository bootstrap behavior.

Implementations MAY populate or synchronize the workspace using implementation-defined logic and/or
hooks (for example `after_create` and/or `before_run`).

Failure handling:

- Workspace population/synchronization failures return an error for the current attempt.
- If failure happens while creating a brand-new workspace, implementations MAY remove the partially
  prepared directory.
- Reused workspaces SHOULD NOT be destructively reset on population failure unless that policy is
  explicitly chosen and documented.

### 9.4 Workspace Hooks

Supported hooks:

- `hooks.after_create`
- `hooks.before_run`
- `hooks.after_run`
- `hooks.before_remove`

Execution contract:

- Execute in a local shell context appropriate to the host OS, with the workspace directory as
  `cwd`.
- On POSIX systems, `sh -lc <script>` (or a stricter equivalent such as `bash -lc <script>`) is a
  conforming default.
- Hook timeout uses `hooks.timeout_ms`; default: `60000 ms`.
- Log hook start, failures, and timeouts.

Failure semantics:

- `after_create` failure or timeout is fatal to workspace creation.
- `before_run` failure or timeout is fatal to the current run attempt.
- `after_run` failure or timeout is logged and ignored.
- `before_remove` failure or timeout is logged and ignored.

### 9.5 Safety Invariants

This is the most important portability constraint.

Invariant 1: The agent's workspace context MUST be the per-issue workspace
path.

- For subprocess backends (Codex, Claude Code), validate `cwd ==
  workspace_path` before launching the process.
- For in-process HTTP backends (OpenAI-compat, Anthropic Messages),
  every filesystem-touching tool dispatch MUST validate that the paths it
  operates on resolve under `workspace_path`. The tool layer is the
  enforcement point because the agent never runs as a subprocess.

Invariant 2: Workspace path MUST stay inside workspace root.

- Normalize both paths to absolute.
- Require `workspace_path` to have `workspace_root` as a prefix directory.
- Reject any path outside the workspace root.

Invariant 3: Workspace key is sanitized.

- Only `[A-Za-z0-9._-]` allowed in workspace directory names.
- Replace all other characters with `_`.

## 10. Agent Runner Protocol

Symphony's orchestration responsibilities — polling, dispatch, retries,
reconciliation, workspace isolation, hook execution, observability — are
independent of which agent runtime actually does the per-issue work. The
"agent runner" is whatever Symphony delegates to: a subprocess speaking a
JSON-RPC stdio protocol (Codex, Claude Code), or an in-process loop talking
to a hosted LLM endpoint (Anthropic Messages, OpenAI-compatible APIs such as
Kimi K2 and GLM).

This section defines:

- The generic contract every backend MUST satisfy (§10.1–§10.6).
- Per-backend implementation requirements (§10.A–§10.D).

The provider-specific protocol (Codex app-server schema, Claude Code
stream-json, OpenAI chat-completions, Anthropic Messages) is the source of
truth for that backend's wire format. Where this specification appears to
conflict with a vendor protocol, the vendor protocol controls wire shape and
the Symphony-specific requirements in this section still control
orchestration behavior, workspace selection, prompt construction, continuation
handling, and observability extraction.

### 10.1 Generic Agent Runner Contract

Every backend MUST provide a client that satisfies the following operations,
regardless of transport:

1. `start_session(workspace_path, policies)`
   - Establish whatever protocol-level context is needed (handshake, system
     prompt, tool advertisement, etc.) in the per-issue workspace.
   - Return an opaque session identifier suitable for re-use across
     continuation turns within the same worker run.

2. `run_turn(session, prompt, issue_metadata)`
   - Execute one agent turn driven by the rendered prompt.
   - Stream `RuntimeEvent`s to the orchestrator (see §10.3).
   - Resolve to `success`, `failure`, or `cancelled`.

3. `stop_session(session)`
   - Tear down the session cleanly. For subprocess backends, this MUST
     cause the child process to exit; for HTTP backends, MUST release any
     held resources / cancel in-flight requests.

Mandatory invariants (all backends):

- Workspace cwd discipline (SPEC §9.5):
  - For subprocess backends, the OS cwd of the spawned process MUST be the
    per-issue workspace path.
  - For in-process HTTP backends, every tool call that exposes filesystem
    access MUST be evaluated against the per-issue workspace path
    (`workspace_path` enforcement happens in the tool handler).
- Continuation pattern (SPEC §7.1):
  - The first turn MUST send the full rendered task prompt.
  - Subsequent in-worker turns MUST reuse the live session and SHOULD send
    only continuation guidance, not resend the original prompt.
  - The same session SHOULD remain alive across continuation turns and
    SHOULD be stopped only when the worker run is ending.
- Issue-identifying metadata, such as `<issue.identifier>: <issue.title>`,
  SHOULD be attached to whatever session/turn label field the backend
  exposes.
- Tool dispatch and approval handling MUST follow §10.4 — unsupported tool
  calls return failure (rather than stalling), user-input-required signals
  resolve in finite time per the implementation's documented policy.
- Token usage and rate-limit telemetry SHOULD be extracted into the
  RuntimeEvent stream so the orchestrator can update aggregate totals
  (SPEC §13.5).

Session identifiers:

- Backends that expose a thread + turn identity (Codex, Claude Code) MUST
  emit `session_id = "<thread_id>-<turn_id>"` and reuse the same `thread_id`
  for all continuation turns inside one worker run.
- Backends without a native thread identity (HTTP chat-completions,
  Anthropic Messages) SHOULD synthesize a stable per-worker `thread_id`
  (e.g. a UUID generated at session start) and a `turn_id` per turn so
  observability fields stay consistent.

Workspace policy translation:

- Each backend translates Symphony's `SessionPolicies` into its own
  approval / sandbox / permission model. The §5.3.6 backend-specific
  config blocks define how those values are surfaced.

### 10.2 Streaming Turn Processing

A turn proceeds until exactly one of the following is observed:

- Backend-specific turn completion signal → success
- Backend-specific turn failure signal → failure
- Backend-specific turn cancellation signal → failure
- `turn_timeout_ms` elapsed → failure
- Transport / subprocess termination → failure

Continuation processing:

- If the worker decides to continue after a successful turn, it SHOULD
  start another turn on the same live session.
- The session (subprocess or HTTP context) SHOULD remain alive across
  continuation turns and SHOULD be stopped only when the worker run is
  ending.

Transport handling requirements:

- Follow the wire and framing rules of the targeted backend.
- For stdio-based transports, keep protocol stream handling separate from
  diagnostic stderr handling unless the targeted protocol specifies
  otherwise.

### 10.3 Emitted Runtime Events (Upstream to Orchestrator)

Every backend client emits structured events to the orchestrator callback.
Each event SHOULD include:

- `event` (enum/string)
- `timestamp` (UTC timestamp)
- `agent_runner_pid` (if available; null for in-process HTTP backends)
- OPTIONAL `usage` map (token counts)
- payload fields as needed

The following event names form the cross-backend vocabulary:

- `session_started`
- `startup_failed`
- `turn_completed`
- `turn_failed`
- `turn_cancelled`
- `turn_ended_with_error`
- `turn_input_required`
- `approval_auto_approved`
- `unsupported_tool_call`
- `notification`
- `other_message`
- `malformed`

A backend MAY emit additional event names; the orchestrator treats unknown
event strings as observability-only.

### 10.4 Approval, Tool Calls, and User Input Policy

Approval, sandbox, and user-input behavior is implementation-defined.

Policy requirements:

- Each implementation MUST document its chosen approval, sandbox, and
  operator-confirmation posture for every backend it supports.
- Approval requests and user-input-required events MUST NOT leave a run
  stalled indefinitely. An implementation MAY either satisfy them, surface
  them to an operator, auto-resolve them, or fail the run according to its
  documented policy.

Example high-trust behavior:

- Auto-approve command execution approvals for the session.
- Auto-approve file-change approvals for the session.
- Treat user-input-required turns as hard failure.

Unsupported dynamic tool calls:

- Supported dynamic tool calls that are explicitly implemented and
  advertised by the runtime SHOULD be handled according to their
  extension contract.
- If the agent requests a dynamic tool call that is not supported, return
  a tool failure response using the backend's protocol and continue the
  session.
- This prevents the session from stalling on unsupported tool execution
  paths.

Optional client-side tool extension:

- An implementation MAY expose a limited set of client-side tools.
- Current standardized optional tool: `linear_graphql`.
- If implemented, supported tools SHOULD be advertised to the session
  during startup using whichever mechanism the selected backend exposes:
  `dynamicTools` (Codex), `tools` (Claude Code stream-json), `tools`
  (OpenAI chat-completions), `tools` (Anthropic Messages).
- Unsupported tool names SHOULD still return a failure result and continue
  the session.

`linear_graphql` extension contract:

- Purpose: execute a raw GraphQL query or mutation against Linear using
  Symphony's configured tracker auth for the current session.
- Availability: only meaningful when `tracker.kind == "linear"` and valid
  Linear auth is configured.
- Preferred input shape:

  ```json
  {
    "query": "single GraphQL query or mutation document",
    "variables": {
      "optional": "graphql variables object"
    }
  }
  ```

- `query` MUST be a non-empty string.
- `query` MUST contain exactly one GraphQL operation.
- `variables` is OPTIONAL and, when present, MUST be a JSON object.
- Implementations MAY additionally accept a raw GraphQL query string as
  shorthand input.
- Execute one GraphQL operation per tool call.
- If the provided document contains multiple operations, reject the tool
  call as invalid input.
- `operationName` selection is intentionally out of scope for this
  extension.
- Reuse the configured Linear endpoint and auth from the active Symphony
  workflow/runtime config; do not require the agent to read raw tokens
  from disk.
- Tool result semantics:
  - transport success + no top-level GraphQL `errors` → `success=true`
  - top-level GraphQL `errors` present → `success=false`, but preserve the
    GraphQL response body for debugging
  - invalid input, missing auth, or transport failure → `success=false`
    with an error payload
- Return the GraphQL response or error payload as structured tool output
  that the model can inspect in-session.

User-input-required policy:

- Implementations MUST document how user-input-required signals are
  handled per backend.
- A run MUST NOT stall indefinitely waiting for user input.
- A conforming implementation MAY fail the run, surface the request to an
  operator, satisfy it through an approved operator channel, or
  auto-resolve it according to its documented policy.
- The example high-trust behavior above fails user-input-required turns
  immediately.

### 10.5 Timeouts and Error Mapping

Timeouts (per backend block; see §5.3.6):

- `<backend>.read_timeout_ms`: request/response timeout during startup
  and synchronous calls.
- `<backend>.turn_timeout_ms`: total turn budget.
- `<backend>.stall_timeout_ms`: orchestrator-enforced inactivity window.

Error mapping (RECOMMENDED normalized categories — apply to every backend):

- `agent_runner_not_found` (subprocess CLI / HTTP endpoint unreachable)
- `invalid_workspace_cwd`
- `response_timeout`
- `turn_timeout`
- `port_exit` (subprocess transports only)
- `response_error`
- `turn_failed`
- `turn_cancelled`
- `turn_input_required`
- `auth_error` (HTTP backends; e.g. 401/403 from the provider)
- `quota_exceeded` (HTTP backends; e.g. 429 with retry semantics)

### 10.6 Agent Runner Wrapper

The `Agent Runner` wraps workspace + prompt + backend client.

Behavior:

1. Create/reuse workspace for issue.
2. Build prompt from workflow template.
3. Start backend session.
4. Forward backend events to orchestrator.
5. On any error, fail the worker attempt (the orchestrator will retry).

Note:

- Workspaces are intentionally preserved after successful runs.

### 10.A Codex stdio app-server backend

Reference: https://developers.openai.com/codex/app-server/

Used when `agent.backend == "codex"`. Drives the Codex app-server JSON-RPC
stdio protocol.

Launch contract:

- Command: `codex.command`
- Invocation: `bash -lc <codex.command>`
- Working directory: per-issue workspace path
- Transport: line-delimited JSON over stdio
- RECOMMENDED max line size: 10 MB

Session startup MUST:

- Initialize the app-server session using the targeted Codex protocol
  (`initialize` → `initialized` notification → `thread/start`).
- Supply the absolute per-issue workspace path as `cwd` wherever the
  Codex protocol accepts it (typically `thread/start.params.cwd` and
  `turn/start.params.cwd`).
- Surface `codex.approval_policy`, `codex.thread_sandbox`, and
  `codex.turn_sandbox_policy` as pass-through values to the matching
  Codex protocol fields.
- Advertise client-side tools via `dynamicTools` on `thread/start`.

Session identifiers:

- Extract `thread_id` from the `thread/start` response.
- Extract `turn_id` from each `turn/start` response.
- Emit `session_id = "<thread_id>-<turn_id>"`.

Turn lifecycle is driven by the Codex method names: `turn/completed`,
`turn/failed`, `turn/cancelled`, `item/commandExecution/requestApproval`,
`item/tool/call`, `execCommandApproval`, `applyPatchApproval`,
`thread/tokenUsage/updated`. The implementation's auto-approve posture
SHOULD reply with `acceptForSession` for command-execution approvals and
`approved_for_session` for file-change approvals when configured for
high-trust environments.

### 10.B Claude Code stdio backend

Used when `agent.backend == "claude_code"`. Drives Anthropic's Claude Code
CLI in stream-json mode.

Reference:
https://docs.anthropic.com/en/docs/build-with-claude/claude-code/sdk
(see "stream-json input/output").

Launch contract:

- Command: `claude_code.command`. Default:
  `claude --print --output-format stream-json --input-format stream-json --verbose`.
- Invocation: `bash -lc <claude_code.command>`
- Working directory: per-issue workspace path
- Transport: line-delimited JSON over stdio
- RECOMMENDED max line size: 10 MB

Session startup MUST:

- Send the rendered initial prompt as the first stream-json message.
- Pass the workspace path via `--add-dir <workspace>` (also possible via
  `cwd` on the spawned process; both SHOULD agree).
- Map `claude_code.permission_mode` to the `--permission-mode` argument
  and `claude_code.allowed_tools` / `disallowed_tools` to the
  corresponding flags.
- Advertise the client-side `tools` set in the stream-json `system`
  message that the SDK accepts.

Continuation pattern:

- The Claude Code CLI keeps a single session alive while stdin is open.
  Continuation turns SHOULD be sent as additional `user` messages on the
  same stdin stream.

Session identifiers:

- Extract `thread_id` from the `system` init message's `session_id` field.
- Synthesize `turn_id` as a per-turn monotonic counter (1-based).
- Emit `session_id = "<thread_id>-<turn_id>"`.

Turn lifecycle:

- The CLI emits `assistant`, `user`, `result`, and `system` stream-json
  messages. Map `result` (`subtype: "success" | "error_max_turns" |
  "error_during_execution"`) onto Symphony's turn outcomes (success,
  failure, failure respectively). Per-message `tool_use` requests follow
  §10.4 tool dispatch.

### 10.C OpenAI-compatible HTTP backend

Used when `agent.backend == "openai_compat"`. Drives any provider that
exposes a chat-completions endpoint compatible with OpenAI's
`POST /v1/chat/completions` schema. Verified targets include OpenAI itself,
Moonshot Kimi (`https://api.moonshot.ai/v1`), Zhipu GLM
(`https://open.bigmodel.cn/api/paas/v4`), DeepSeek
(`https://api.deepseek.com/v1`), and self-hosted vLLM / llama.cpp servers.

Launch contract:

- No subprocess. The session is an in-process HTTP client.
- Workspace cwd discipline is enforced by the tool dispatch layer rather
  than the OS — every filesystem-touching tool MUST validate that the
  paths it operates on stay within the per-issue workspace path.

Session startup MUST:

- Capture the rendered prompt as the first user message in the
  conversation history.
- Optionally prepend a system message derived from `WORKFLOW.md`
  semantics or a backend-specific `system` config field.
- Advertise client-side tools (e.g. `linear_graphql`) using the
  `tools` array on every chat-completions request.

Per-turn loop:

1. POST `<endpoint>/chat/completions` with the conversation so far,
   `model`, `tools`, and any `extra_request_fields` merged in.
2. Stream the response if the provider supports `stream: true`; otherwise
   read the full body. Emit `notification` events for any reasoning /
   content deltas observed.
3. If the response message contains `tool_calls`, dispatch each via
   §10.4, append the tool result message to the conversation, and POST
   another chat-completions request. This loop repeats until the model
   returns `finish_reason: "stop"` (success) or `length` /
   `content_filter` (failure).

Session identifiers:

- Synthesize `thread_id` as a UUID generated at session start.
- `turn_id` is the per-turn 1-based counter.

Authentication:

- HTTP `Authorization: Bearer <openai_compat.api_key>` by default. Some
  providers require additional headers (e.g. `x-bce-token`); use
  `openai_compat.extra_headers` to pass them through.

Vendor presets (provided as configuration examples; not separate
backends):

- Kimi K2: `endpoint: https://api.moonshot.ai/v1`, `model: kimi-k2`.
- GLM: `endpoint: https://open.bigmodel.cn/api/paas/v4`, `model: glm-4.6`.
- DeepSeek: `endpoint: https://api.deepseek.com/v1`,
  `model: deepseek-chat`.

Token usage MUST be extracted from the `usage` object in the response
body (`prompt_tokens`, `completion_tokens`, `total_tokens`). Rate-limit
information MAY be extracted from response headers
(`x-ratelimit-limit-*`, `retry-after`).

### 10.D Anthropic Messages HTTP backend

Used when `agent.backend == "anthropic_messages"`. Drives the Anthropic
Messages API (`POST /v1/messages`).

Reference: https://docs.anthropic.com/en/api/messages

Launch contract:

- No subprocess. In-process HTTP client.
- Workspace cwd discipline enforced by tool dispatch (same as §10.C).

Session startup MUST:

- Use `anthropic_messages.system` (or a default Symphony-derived system
  prompt) as the `system` field on every request.
- Capture the rendered prompt as the first user message.
- Advertise client-side tools using the Messages API `tools` array.

Per-turn loop:

1. POST `<endpoint>/messages` with `model`, `max_tokens`, `system`,
   `messages`, and `tools`.
2. Stream the response when supported. Emit `notification` events for
   `content_block_delta` and `message_delta` events.
3. If the assistant message contains `tool_use` blocks, dispatch each via
   §10.4, append matching `tool_result` blocks to the conversation, and
   POST another request. This loop repeats until `stop_reason: "end_turn"`
   (success) or `max_tokens` / `tool_use` failure / `error` (failure).

Authentication:

- HTTP `x-api-key: <anthropic_messages.api_key>` and
  `anthropic-version: 2023-06-01` by default. Override / extend via
  `anthropic_messages.extra_headers`.

Token usage MUST be extracted from the `usage` field of the response
(`input_tokens`, `output_tokens`, plus `cache_creation_input_tokens` /
`cache_read_input_tokens` when present). The Anthropic API surfaces
rate-limit data via response headers (`anthropic-ratelimit-*`).

Session identifiers:

- Synthesize `thread_id` as a UUID generated at session start.
- `turn_id` is the per-turn 1-based counter.

## 11. Issue Tracker Integration Contract (Linear-Compatible)

### 11.1 REQUIRED Operations

An implementation MUST support these tracker adapter operations:

1. `fetch_candidate_issues()`
   - Return issues in configured active states for a configured project.

2. `fetch_issues_by_states(state_names)`
   - Used for startup terminal cleanup.

3. `fetch_issue_states_by_ids(issue_ids)`
   - Used for active-run reconciliation.

### 11.2 Query Semantics (Linear)

Linear-specific requirements for `tracker.kind == "linear"`:

- `tracker.kind == "linear"`
- GraphQL endpoint (default `https://api.linear.app/graphql`)
- Auth token sent in `Authorization` header
- `tracker.project_slug` maps to Linear project `slugId`
- Candidate issue query filters project using `project: { slugId: { eq: $projectSlug } }`
- Issue-state refresh query uses GraphQL issue IDs with variable type `[ID!]`
- Pagination REQUIRED for candidate issues
- Page size default: `50`
- Network timeout: `30000 ms`

Important:

- Linear GraphQL schema details can drift. Keep query construction isolated and test the exact query
  fields/types REQUIRED by this specification.

A non-Linear implementation MAY change transport details, but the normalized outputs MUST match the
domain model in Section 4.

### 11.3 Normalization Rules

Candidate issue normalization SHOULD produce fields listed in Section 4.1.1.

Additional normalization details:

- `labels` -> lowercase strings
- `blocked_by` -> derived from inverse relations where relation type is `blocks`
- `priority` -> integer only (non-integers become null)
- `created_at` and `updated_at` -> parse ISO-8601 timestamps

### 11.4 Error Handling Contract

RECOMMENDED error categories:

- `unsupported_tracker_kind`
- `missing_tracker_api_key`
- `missing_tracker_project_slug`
- `linear_api_request` (transport failures)
- `linear_api_status` (non-200 HTTP)
- `linear_graphql_errors`
- `linear_unknown_payload`
- `linear_missing_end_cursor` (pagination integrity error)

Orchestrator behavior on tracker errors:

- Candidate fetch failure: log and skip dispatch for this tick.
- Running-state refresh failure: log and keep active workers running.
- Startup terminal cleanup failure: log warning and continue startup.

### 11.5 Tracker Writes (Important Boundary)

Symphony does not require first-class tracker write APIs in the orchestrator.

- Ticket mutations (state transitions, comments, PR metadata) are typically handled by the coding
  agent using tools defined by the workflow prompt.
- The service remains a scheduler/runner and tracker reader.
- Workflow-specific success often means "reached the next handoff state" (for example
  `Human Review`) rather than tracker terminal state `Done`.
- If the `linear_graphql` client-side tool extension is implemented, it is still part of the agent
  toolchain rather than orchestrator business logic.

## 12. Prompt Construction and Context Assembly

### 12.1 Inputs

Inputs to prompt rendering:

- `workflow.prompt_template`
- normalized `issue` object
- OPTIONAL `attempt` integer (retry/continuation metadata)

### 12.2 Rendering Rules

- Render with strict variable checking.
- Render with strict filter checking.
- Convert issue object keys to strings for template compatibility.
- Preserve nested arrays/maps (labels, blockers) so templates can iterate.

### 12.3 Retry/Continuation Semantics

`attempt` SHOULD be passed to the template because the workflow prompt can provide different
instructions for:

- first run (`attempt` null or absent)
- continuation run after a successful prior session
- retry after error/timeout/stall

### 12.4 Failure Semantics

If prompt rendering fails:

- Fail the run attempt immediately.
- Let the orchestrator treat it like any other worker failure and decide retry behavior.

## 13. Logging, Status, and Observability

### 13.1 Logging Conventions

REQUIRED context fields for issue-related logs:

- `issue_id`
- `issue_identifier`

REQUIRED context for agent session lifecycle logs:

- `session_id`

Message formatting requirements:

- Use stable `key=value` phrasing.
- Include action outcome (`completed`, `failed`, `retrying`, etc.).
- Include concise failure reason when present.
- Avoid logging large raw payloads unless necessary.

### 13.2 Logging Outputs and Sinks

The spec does not prescribe where logs are written (stderr, file, remote sink, etc.).

Requirements:

- Operators MUST be able to see startup/validation/dispatch failures without attaching a debugger.
- Implementations MAY write to one or more sinks.
- If a configured log sink fails, the service SHOULD continue running when possible and emit an
  operator-visible warning through any remaining sink.

### 13.3 Runtime Snapshot / Monitoring Interface (OPTIONAL but RECOMMENDED)

If the implementation exposes a synchronous runtime snapshot (for dashboards or monitoring), it
SHOULD return:

- `running` (list of running session rows)
- each running row SHOULD include `turn_count`
- `retrying` (list of retry queue rows)
- `agent_totals`
  - `input_tokens`
  - `output_tokens`
  - `total_tokens`
  - `seconds_running` (aggregate runtime seconds as of snapshot time, including active sessions)
- `rate_limits` (latest agent backend rate limit payload, if available)

RECOMMENDED snapshot error modes:

- `timeout`
- `unavailable`

### 13.4 OPTIONAL Human-Readable Status Surface

A human-readable status surface (terminal output, dashboard, etc.) is OPTIONAL and
implementation-defined.

If present, it SHOULD draw from orchestrator state/metrics only and MUST NOT be REQUIRED for
correctness.

### 13.5 Session Metrics and Token Accounting

Token accounting rules:

- Agent events can include token counts in multiple payload shapes.
- Prefer absolute thread totals when available, such as:
  - `thread/tokenUsage/updated` payloads
  - `total_token_usage` within token-count wrapper events
- Ignore delta-style payloads such as `last_token_usage` for dashboard/API totals.
- Extract input/output/total token counts leniently from common field names within the selected
  payload.
- For absolute totals, track deltas relative to last reported totals to avoid double-counting.
- Do not treat generic `usage` maps as cumulative totals unless the event type defines them that
  way.
- Accumulate aggregate totals in orchestrator state.

Runtime accounting:

- Runtime SHOULD be reported as a live aggregate at snapshot/render time.
- Implementations MAY maintain a cumulative counter for ended sessions and add active-session
  elapsed time derived from `running` entries (for example `started_at`) when producing a
  snapshot/status view.
- Add run duration seconds to the cumulative ended-session runtime when a session ends (normal exit
  or cancellation/termination).
- Continuous background ticking of runtime totals is not REQUIRED.

Rate-limit tracking:

- Track the latest rate-limit payload seen in any agent update.
- Any human-readable presentation of rate-limit data is implementation-defined.

### 13.6 Humanized Agent Event Summaries (OPTIONAL)

Humanized summaries of raw agent protocol events are OPTIONAL.

If implemented:

- Treat them as observability-only output.
- Do not make orchestrator logic depend on humanized strings.

### 13.7 OPTIONAL HTTP Server Extension

This section defines an OPTIONAL HTTP interface for observability and operational control.

If implemented:

- The HTTP server is an extension and is not REQUIRED for conformance.
- The implementation MAY serve server-rendered HTML or a client-side application for the dashboard.
- The dashboard/API MUST be observability/control surfaces only and MUST NOT become REQUIRED for
  orchestrator correctness.

Extension config:

- `server.port` (integer, OPTIONAL)
  - Enables the HTTP server extension.
  - `0` requests an ephemeral port for local development and tests.
  - CLI `--port` overrides `server.port` when both are present.

Enablement (extension):

- Start the HTTP server when a CLI `--port` argument is provided.
- Start the HTTP server when `server.port` is present in `WORKFLOW.md` front matter.
- The `server` top-level key is owned by this extension.
- Positive `server.port` values bind that port.
- Implementations SHOULD bind loopback by default (`127.0.0.1` or host equivalent) unless explicitly
  configured otherwise.
- Changes to HTTP listener settings (for example `server.port`) do not need to hot-rebind;
  restart-required behavior is conformant.

#### 13.7.1 Human-Readable Dashboard (`/`)

- Host a human-readable dashboard at `/`.
- The returned document SHOULD depict the current state of the system (for example active sessions,
  retry delays, token consumption, runtime totals, recent events, and health/error indicators).
- It is up to the implementation whether this is server-generated HTML or a client-side app that
  consumes the JSON API below.

#### 13.7.2 JSON REST API (`/api/v1/*`)

Provide a JSON REST API under `/api/v1/*` for current runtime state and operational debugging.

Minimum endpoints:

- `GET /api/v1/state`
  - Returns a summary view of the current system state (running sessions, retry queue/delays,
    aggregate token/runtime totals, latest rate limits, and any additional tracked summary fields).
  - Suggested response shape:

    ```json
    {
      "generated_at": "2026-02-24T20:15:30Z",
      "counts": {
        "running": 2,
        "retrying": 1
      },
      "running": [
        {
          "issue_id": "abc123",
          "issue_identifier": "MT-649",
          "state": "In Progress",
          "session_id": "thread-1-turn-1",
          "turn_count": 7,
          "last_event": "turn_completed",
          "last_message": "",
          "started_at": "2026-02-24T20:10:12Z",
          "last_event_at": "2026-02-24T20:14:59Z",
          "tokens": {
            "input_tokens": 1200,
            "output_tokens": 800,
            "total_tokens": 2000
          }
        }
      ],
      "retrying": [
        {
          "issue_id": "def456",
          "issue_identifier": "MT-650",
          "attempt": 3,
          "due_at": "2026-02-24T20:16:00Z",
          "error": "no available orchestrator slots"
        }
      ],
      "agent_totals": {
        "input_tokens": 5000,
        "output_tokens": 2400,
        "total_tokens": 7400,
        "seconds_running": 1834.2
      },
      "rate_limits": null
    }
    ```

- `GET /api/v1/<issue_identifier>`
  - Returns issue-specific runtime/debug details for the identified issue, including any information
    the implementation tracks that is useful for debugging.
  - Suggested response shape:

    ```json
    {
      "issue_identifier": "MT-649",
      "issue_id": "abc123",
      "status": "running",
      "workspace": {
        "path": "/tmp/symphony_workspaces/MT-649"
      },
      "attempts": {
        "restart_count": 1,
        "current_retry_attempt": 2
      },
      "running": {
        "session_id": "thread-1-turn-1",
        "turn_count": 7,
        "state": "In Progress",
        "started_at": "2026-02-24T20:10:12Z",
        "last_event": "notification",
        "last_message": "Working on tests",
        "last_event_at": "2026-02-24T20:14:59Z",
        "tokens": {
          "input_tokens": 1200,
          "output_tokens": 800,
          "total_tokens": 2000
        }
      },
      "retry": null,
      "logs": {
        "agent_session_logs": [
          {
            "label": "latest",
            "path": "/var/log/symphony/agent/MT-649/latest.log",
            "url": null
          }
        ]
      },
      "recent_events": [
        {
          "at": "2026-02-24T20:14:59Z",
          "event": "notification",
          "message": "Working on tests"
        }
      ],
      "last_error": null,
      "tracked": {}
    }
    ```

  - If the issue is unknown to the current in-memory state, return `404` with an error response (for
    example `{\"error\":{\"code\":\"issue_not_found\",\"message\":\"...\"}}`).

- `POST /api/v1/refresh`
  - Queues an immediate tracker poll + reconciliation cycle (best-effort trigger; implementations
    MAY coalesce repeated requests).
  - Suggested request body: empty body or `{}`.
  - Suggested response (`202 Accepted`) shape:

    ```json
    {
      "queued": true,
      "coalesced": false,
      "requested_at": "2026-02-24T20:15:30Z",
      "operations": ["poll", "reconcile"]
    }
    ```

API design notes:

- The JSON shapes above are the RECOMMENDED baseline for interoperability and debugging ergonomics.
- Implementations MAY add fields, but SHOULD avoid breaking existing fields within a version.
- Endpoints SHOULD be read-only except for operational triggers like `/refresh`.
- Unsupported methods on defined routes SHOULD return `405 Method Not Allowed`.
- API errors SHOULD use a JSON envelope such as `{"error":{"code":"...","message":"..."}}`.
- If the dashboard is a client-side app, it SHOULD consume this API rather than duplicating state
  logic.

#### 13.7.3 OPTIONAL operator-control endpoints

If the implementation chooses to surface operator controls beyond
`POST /api/v1/refresh`, the following endpoints are RECOMMENDED for
interoperability:

- `POST /api/v1/<issue_identifier>/retry`
  - Force-schedule a retry for an issue currently tracked in the
    orchestrator's running or retry state.
  - Suggested response: `202 Accepted` with
    `{"queued": true, "issue_identifier": "MT-649", "attempt": 2}`.
  - If the issue is unknown, return `404` with the standard error envelope.
  - This is a hint to the orchestrator, not a workspace mutation; the
    next reconcile tick decides whether the retry actually dispatches
    based on slot availability and tracker state.

- `GET /api/v1/<issue_identifier>/workspace`
  - Returns a read-only listing of files inside the per-issue workspace
    so operators can inspect the agent's working state without SSH.
  - Suggested response shape:

    ```json
    {
      "issue_identifier": "MT-649",
      "workspace_path": "/tmp/symphony_workspaces/MT-649",
      "entries": [
        {"path": "src/main.rs", "size": 1024, "kind": "file"},
        {"path": ".git", "size": 0, "kind": "dir"}
      ],
      "total_bytes": 12345
    }
    ```
  - Implementations MUST validate path-traversal queries (no `..`),
    SHOULD truncate large directory listings, and SHOULD NOT serve
    binary blobs over this endpoint by default.

- `GET /api/v1/<issue_identifier>/workspace/<file_path>`
  - Returns the raw bytes of one file under the per-issue workspace,
    enforcing the §9.5 root-prefix containment invariant.
  - Implementations SHOULD cap response size, set
    `Content-Type: text/plain; charset=utf-8` when the file is detected
    as text, and `application/octet-stream` otherwise.

#### 13.7.4 OPTIONAL live event stream

The snapshot polling model in §13.7.2 is sufficient for control surfaces
but does not give operators a "watch the agent work" experience. An
implementation MAY expose a Server-Sent Events stream:

- `GET /api/v1/events`
  - `Content-Type: text/event-stream`.
  - Each event SHOULD have `event:` set to the `RuntimeEvent.event`
    string (`session_started`, `turn_completed`, `notification`, etc.)
    and `data:` set to a JSON object containing at minimum
    `issue_identifier`, `session_id`, `timestamp`, and any payload
    fields the implementation chooses to forward.
  - Implementations SHOULD include an initial `event: snapshot` carrying
    the same payload as `GET /api/v1/state` so newly-connected clients
    can render immediately.
  - Backpressure: if a subscriber falls behind, the implementation MAY
    drop events for that subscriber rather than buffer indefinitely; a
    dropped notice (`event: lagged`) SHOULD be sent so the client knows
    to re-snapshot.
  - This stream is observability-only and MUST NOT become required for
    orchestrator correctness.

## 14. Failure Model and Recovery Strategy

### 14.1 Failure Classes

1. `Workflow/Config Failures`
   - Missing `WORKFLOW.md`
   - Invalid YAML front matter
   - Unsupported tracker kind or missing tracker credentials/project slug
   - Missing or unsupported `agent.backend`
   - Missing agent runner prerequisites for the selected backend (e.g.
     `codex` / `claude` binary not on PATH for subprocess backends, missing
     API key for HTTP backends)

2. `Workspace Failures`
   - Workspace directory creation failure
   - Workspace population/synchronization failure (implementation-defined; can come from hooks)
   - Invalid workspace path configuration
   - Hook timeout/failure

3. `Agent Session Failures`
   - Startup handshake / authentication failure
   - Turn failed/cancelled
   - Turn timeout
   - User input requested and handled as failure by the implementation's
     documented policy
   - Subprocess exit (subprocess backends) or transport disconnect / 5xx
     (HTTP backends)
   - Provider-side rate limiting / quota exhaustion (HTTP backends)
   - Stalled session (no activity)

4. `Tracker Failures`
   - API transport errors
   - Non-200 status
   - GraphQL errors
   - malformed payloads

5. `Observability Failures`
   - Snapshot timeout
   - Dashboard render errors
   - Log sink configuration failure

### 14.2 Recovery Behavior

- Dispatch validation failures:
  - Skip new dispatches.
  - Keep service alive.
  - Continue reconciliation where possible.

- Worker failures:
  - Convert to retries with exponential backoff.

- Tracker candidate-fetch failures:
  - Skip this tick.
  - Try again on next tick.

- Reconciliation state-refresh failures:
  - Keep current workers.
  - Retry on next tick.

- Dashboard/log failures:
  - Do not crash the orchestrator.

### 14.3 Partial State Recovery (Restart)

Current design is intentionally in-memory for scheduler state.
Restart recovery means the service can resume useful operation by polling tracker state and reusing
preserved workspaces. It does not mean retry timers, running sessions, or live worker state survive
process restart.

After restart:

- No retry timers are restored from prior process memory.
- No running sessions are assumed recoverable.
- Service recovers by:
  - startup terminal workspace cleanup
  - fresh polling of active issues
  - re-dispatching eligible work

### 14.4 Operator Intervention Points

Operators can control behavior by:

- Editing `WORKFLOW.md` (prompt and most runtime settings).
- `WORKFLOW.md` changes are detected and re-applied automatically without restart according to
  Section 6.2.
- Changing issue states in the tracker:
  - terminal state -> running session is stopped and workspace cleaned when reconciled
  - non-active state -> running session is stopped without cleanup
- Restarting the service for process recovery or deployment (not as the normal path for applying
  workflow config changes).

## 15. Security and Operational Safety

### 15.1 Trust Boundary Assumption

Each implementation defines its own trust boundary.

Operational safety requirements:

- Implementations SHOULD state clearly whether they are intended for trusted environments, more
  restrictive environments, or both.
- Implementations SHOULD state clearly whether they rely on auto-approved actions, operator
  approvals, stricter sandboxing, or some combination of those controls.
- Workspace isolation and path validation are important baseline controls, but they are not a
  substitute for whatever approval and sandbox policy an implementation chooses.

### 15.2 Filesystem Safety Requirements

Mandatory:

- Workspace path MUST remain under configured workspace root.
- Coding-agent cwd MUST be the per-issue workspace path for the current run.
- Workspace directory names MUST use sanitized identifiers.

RECOMMENDED additional hardening for ports:

- Run under a dedicated OS user.
- Restrict workspace root permissions.
- Mount workspace root on a dedicated volume if possible.

### 15.3 Secret Handling

- Support `$VAR` indirection in workflow config.
- Do not log API tokens or secret env values.
- Validate presence of secrets without printing them.

### 15.4 Hook Script Safety

Workspace hooks are arbitrary shell scripts from `WORKFLOW.md`.

Implications:

- Hooks are fully trusted configuration.
- Hooks run inside the workspace directory.
- Hook output SHOULD be truncated in logs.
- Hook timeouts are REQUIRED to avoid hanging the orchestrator.

### 15.5 Harness Hardening Guidance

Running coding agents against repositories, issue trackers, and other
inputs that can contain sensitive data or externally-controlled content can
be dangerous. A permissive deployment — regardless of which backend the
implementation has selected — can lead to data leaks, destructive
mutations, or full machine compromise if the agent is induced to execute
harmful commands or use overly-powerful integrations.

Implementations SHOULD explicitly evaluate their own risk profile and harden the execution harness
where appropriate. This specification intentionally does not mandate a single hardening posture, but
implementations SHOULD NOT assume that tracker data, repository contents, prompt inputs, or tool
arguments are fully trustworthy just because they originate inside a normal workflow.

Possible hardening measures include:

- Tightening backend-specific approval, sandbox, or permission settings
  described elsewhere in this specification instead of running with a
  maximally permissive configuration. (For Codex: `approval_policy`,
  `thread_sandbox`, `turn_sandbox_policy`. For Claude Code: `permission_mode`,
  `allowed_tools`. For HTTP backends: tool-handler whitelisting.)
- Adding external isolation layers such as OS/container/VM sandboxing,
  network restrictions, or separate credentials beyond the built-in
  policy controls of any single backend.
- Filtering which Linear issues, projects, teams, labels, or other tracker sources are eligible for
  dispatch so untrusted or out-of-scope tasks do not automatically reach the agent.
- Narrowing the `linear_graphql` tool so it can only read or mutate data inside the
  intended project scope, rather than exposing general workspace-wide tracker access.
- Reducing the set of client-side tools, credentials, filesystem paths, and network destinations
  available to the agent to the minimum needed for the workflow.

The correct controls are deployment-specific, but implementations SHOULD document them clearly and
treat harness hardening as part of the core safety model rather than an optional afterthought.

## 16. Reference Algorithms (Language-Agnostic)

### 16.1 Service Startup

```text
function start_service():
  configure_logging()
  start_observability_outputs()
  start_workflow_watch(on_change=reload_and_reapply_workflow)

  state = {
    poll_interval_ms: get_config_poll_interval_ms(),
    max_concurrent_agents: get_config_max_concurrent_agents(),
    running: {},
    claimed: set(),
    retry_attempts: {},
    completed: set(),
    agent_totals: {input_tokens: 0, output_tokens: 0, total_tokens: 0, seconds_running: 0},
    agent_rate_limits: null
  }

  validation = validate_dispatch_config()
  if validation is not ok:
    log_validation_error(validation)
    fail_startup(validation)

  startup_terminal_workspace_cleanup()
  schedule_tick(delay_ms=0)

  event_loop(state)
```

### 16.2 Poll-and-Dispatch Tick

```text
on_tick(state):
  state = reconcile_running_issues(state)

  validation = validate_dispatch_config()
  if validation is not ok:
    log_validation_error(validation)
    notify_observers()
    schedule_tick(state.poll_interval_ms)
    return state

  issues = tracker.fetch_candidate_issues()
  if issues failed:
    log_tracker_error()
    notify_observers()
    schedule_tick(state.poll_interval_ms)
    return state

  for issue in sort_for_dispatch(issues):
    if no_available_slots(state):
      break

    if should_dispatch(issue, state):
      state = dispatch_issue(issue, state, attempt=null)

  notify_observers()
  schedule_tick(state.poll_interval_ms)
  return state
```

### 16.3 Reconcile Active Runs

```text
function reconcile_running_issues(state):
  state = reconcile_stalled_runs(state)

  running_ids = keys(state.running)
  if running_ids is empty:
    return state

  refreshed = tracker.fetch_issue_states_by_ids(running_ids)
  if refreshed failed:
    log_debug("keep workers running")
    return state

  for issue in refreshed:
    if issue.state in terminal_states:
      state = terminate_running_issue(state, issue.id, cleanup_workspace=true)
    else if issue.state in active_states:
      state.running[issue.id].issue = issue
    else:
      state = terminate_running_issue(state, issue.id, cleanup_workspace=false)

  return state
```

### 16.4 Dispatch One Issue

```text
function dispatch_issue(issue, state, attempt):
  worker = spawn_worker(
    fn -> run_agent_attempt(issue, attempt, parent_orchestrator_pid) end
  )

  if worker spawn failed:
    return schedule_retry(state, issue.id, next_attempt(attempt), {
      identifier: issue.identifier,
      error: "failed to spawn agent"
    })

  state.running[issue.id] = {
    worker_handle,
    monitor_handle,
    identifier: issue.identifier,
    issue,
    session_id: null,
    agent_runner_pid: null,
    last_agent_message: null,
    last_agent_event: null,
    last_agent_timestamp: null,
    agent_input_tokens: 0,
    agent_output_tokens: 0,
    agent_total_tokens: 0,
    last_reported_input_tokens: 0,
    last_reported_output_tokens: 0,
    last_reported_total_tokens: 0,
    retry_attempt: normalize_attempt(attempt),
    started_at: now_utc()
  }

  state.claimed.add(issue.id)
  state.retry_attempts.remove(issue.id)
  return state
```

### 16.5 Worker Attempt (Workspace + Prompt + Agent)

```text
function run_agent_attempt(issue, attempt, orchestrator_channel):
  workspace = workspace_manager.create_for_issue(issue.identifier)
  if workspace failed:
    fail_worker("workspace error")

  if run_hook("before_run", workspace.path) failed:
    fail_worker("before_run hook error")

  session = app_server.start_session(workspace=workspace.path)
  if session failed:
    run_hook_best_effort("after_run", workspace.path)
    fail_worker("agent session startup error")

  max_turns = config.agent.max_turns
  turn_number = 1

  while true:
    prompt = build_turn_prompt(workflow_template, issue, attempt, turn_number, max_turns)
    if prompt failed:
      app_server.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("prompt error")

    turn_result = app_server.run_turn(
      session=session,
      prompt=prompt,
      issue=issue,
      on_message=(msg) -> send(orchestrator_channel, {codex_update, issue.id, msg})
    )

    if turn_result failed:
      app_server.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("agent turn error")

    refreshed_issue = tracker.fetch_issue_states_by_ids([issue.id])
    if refreshed_issue failed:
      app_server.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("issue state refresh error")

    issue = refreshed_issue[0] or issue

    if issue.state is not active:
      break

    if turn_number >= max_turns:
      break

    turn_number = turn_number + 1

  app_server.stop_session(session)
  run_hook_best_effort("after_run", workspace.path)

  exit_normal()
```

### 16.6 Worker Exit and Retry Handling

```text
on_worker_exit(issue_id, reason, state):
  running_entry = state.running.remove(issue_id)
  state = add_runtime_seconds_to_totals(state, running_entry)

  if reason == normal:
    state.completed.add(issue_id)  # bookkeeping only
    state = schedule_retry(state, issue_id, 1, {
      identifier: running_entry.identifier,
      delay_type: continuation
    })
  else:
    state = schedule_retry(state, issue_id, next_attempt_from(running_entry), {
      identifier: running_entry.identifier,
      error: format("worker exited: %reason")
    })

  notify_observers()
  return state
```

```text
on_retry_timer(issue_id, state):
  retry_entry = state.retry_attempts.pop(issue_id)
  if missing:
    return state

  candidates = tracker.fetch_candidate_issues()
  if fetch failed:
    return schedule_retry(state, issue_id, retry_entry.attempt + 1, {
      identifier: retry_entry.identifier,
      error: "retry poll failed"
    })

  issue = find_by_id(candidates, issue_id)
  if issue is null:
    state.claimed.remove(issue_id)
    return state

  if available_slots(state) == 0:
    return schedule_retry(state, issue_id, retry_entry.attempt + 1, {
      identifier: issue.identifier,
      error: "no available orchestrator slots"
    })

  return dispatch_issue(issue, state, attempt=retry_entry.attempt)
```

## 17. Test and Validation Matrix

A conforming implementation SHOULD include tests that cover the behaviors defined in this
specification.

Validation profiles:

- `Core Conformance`: deterministic tests REQUIRED for all conforming implementations.
- `Extension Conformance`: REQUIRED only for OPTIONAL features that an implementation chooses to
  ship.
- `Real Integration Profile`: environment-dependent smoke/integration checks RECOMMENDED before
  production use.

Unless otherwise noted, Sections 17.1 through 17.7 are `Core Conformance`. Bullets that begin with
`If ... is implemented` are `Extension Conformance`.

### 17.1 Workflow and Config Parsing

- Workflow file path precedence:
  - explicit runtime path is used when provided
  - cwd default is `WORKFLOW.md` when no explicit runtime path is provided
- Workflow file changes are detected and trigger re-read/re-apply without restart
- Invalid workflow reload keeps last known good effective configuration and emits an
  operator-visible error
- Missing `WORKFLOW.md` returns typed error
- Invalid YAML front matter returns typed error
- Front matter non-map returns typed error
- Config defaults apply when OPTIONAL values are missing
- `tracker.kind` validation enforces currently supported kind (`linear`)
- `tracker.api_key` works (including `$VAR` indirection)
- `$VAR` resolution works for tracker API key and path values
- `~` path expansion works
- `agent.backend` is recognized and rejects unknown values
- The selected backend's `command` (Codex / Claude Code) or `endpoint` +
  `model` (HTTP backends) is preserved through config parsing
- Per-state concurrency override map normalizes state names and ignores invalid values
- Prompt template renders `issue` and `attempt`
- Prompt rendering fails on unknown variables (strict mode)

### 17.2 Workspace Manager and Safety

- Deterministic workspace path per issue identifier
- Missing workspace directory is created
- Existing workspace directory is reused
- Existing non-directory path at workspace location is handled safely (replace or fail per
  implementation policy)
- OPTIONAL workspace population/synchronization errors are surfaced
- `after_create` hook runs only on new workspace creation
- `before_run` hook runs before each attempt and failure/timeouts abort the current attempt
- `after_run` hook runs after each attempt and failure/timeouts are logged and ignored
- `before_remove` hook runs on cleanup and failures/timeouts are ignored
- Workspace path sanitization and root containment invariants are enforced before agent launch
- Agent launch uses the per-issue workspace path as cwd and rejects out-of-root paths

### 17.3 Issue Tracker Client

- Candidate issue fetch uses active states and project slug
- Linear query uses the specified project filter field (`slugId`)
- Empty `fetch_issues_by_states([])` returns empty without API call
- Pagination preserves order across multiple pages
- Blockers are normalized from inverse relations of type `blocks`
- Labels are normalized to lowercase
- Issue state refresh by ID returns minimal normalized issues
- Issue state refresh query uses GraphQL ID typing (`[ID!]`) as specified in Section 11.2
- Error mapping for request errors, non-200, GraphQL errors, malformed payloads

### 17.4 Orchestrator Dispatch, Reconciliation, and Retry

- Dispatch sort order is priority then oldest creation time
- `Todo` issue with non-terminal blockers is not eligible
- `Todo` issue with terminal blockers is eligible
- Active-state issue refresh updates running entry state
- Non-active state stops running agent without workspace cleanup
- Terminal state stops running agent and cleans workspace
- Reconciliation with no running issues is a no-op
- Normal worker exit schedules a short continuation retry (attempt 1)
- Abnormal worker exit increments retries with 10s-based exponential backoff
- Retry backoff cap uses configured `agent.max_retry_backoff_ms`
- Retry queue entries include attempt, due time, identifier, and error
- Stall detection kills stalled sessions and schedules retry
- Slot exhaustion requeues retries with explicit error reason
- If a snapshot API is implemented, it returns running rows, retry rows, token totals, and rate
  limits
- If a snapshot API is implemented, timeout/unavailable cases are surfaced

### 17.5 Agent Runner Client

#### 17.5.1 Generic AgentClient (every supported backend)

- Session startup uses the per-issue workspace path as the working context.
- Policy-related startup payloads use the implementation's documented
  approval / sandbox / permission settings.
- The first turn carries the full rendered prompt; subsequent in-worker
  turns reuse the live session and send only continuation guidance.
- Thread and turn identities (native or synthesized) are extracted and
  used to emit `session_started`.
- Request/response read timeout is enforced.
- Turn timeout is enforced.
- Approvals or permission prompts are handled according to the documented
  policy and do not stall indefinitely.
- Unsupported dynamic tool calls are rejected without stalling the session.
- User input requests are handled per the documented policy and resolve
  in finite time.
- Usage and rate-limit telemetry exposed by the backend are extracted.
- If client-side tools are implemented, session startup advertises the
  supported tool specs using the mechanism appropriate to the selected
  backend.
- If the `linear_graphql` client-side tool extension is implemented:
  - the tool is advertised to the session
  - valid `query` / `variables` inputs execute against configured Linear
    auth
  - top-level GraphQL `errors` produce `success=false` while preserving
    the GraphQL body
  - invalid arguments, missing auth, and transport failures return
    structured failure payloads
  - unsupported tool names still fail without stalling the session

#### 17.5.2 Codex stdio backend (when implemented)

- Launch command uses workspace cwd and invokes `bash -lc <codex.command>`.
- Session startup follows the targeted Codex app-server protocol
  (`initialize` → `initialized` → `thread/start`).
- Transport framing required by the targeted protocol is handled correctly.
- Diagnostic stderr handling is kept separate from the JSON-RPC stream.
- Command/file-change approvals are handled per the documented policy.

#### 17.5.3 Claude Code stdio backend (when implemented)

- Launch command uses workspace cwd and invokes
  `bash -lc <claude_code.command>`.
- Session startup writes the rendered initial prompt to stdin and reads
  stream-json messages until a `result` message is observed.
- `--permission-mode`, `--allowed-tools`, `--disallowed-tools`, and
  `--model` reflect the configured `claude_code.*` fields.
- `tool_use` / `tool_result` round-trips dispatch via the generic tool
  contract.

#### 17.5.4 OpenAI-compatible HTTP backend (when implemented)

- POSTs to `<endpoint>/chat/completions` with the configured `model`,
  the conversation so far, and any advertised `tools`.
- `extra_headers` and `extra_request_fields` are merged into outgoing
  requests.
- `tool_calls` in responses dispatch via the generic tool contract; tool
  result messages append to the conversation and the loop continues.
- `usage` token counts are extracted into the RuntimeEvent stream.
- Rate-limit headers (`x-ratelimit-*`, `retry-after`) are surfaced when
  present.
- Filesystem-touching tool handlers validate `workspace_path` enforcement
  per §10.1.

#### 17.5.5 Anthropic Messages HTTP backend (when implemented)

- POSTs to `<endpoint>/messages` with the configured `model`,
  `max_tokens`, `system`, and `tools`.
- `x-api-key` and `anthropic-version` headers are sent by default; can
  be overridden via `extra_headers`.
- `tool_use` blocks dispatch via the generic tool contract; matching
  `tool_result` blocks append to the conversation and the loop continues.
- `usage` token counts (including cache fields when present) are
  extracted.
- `anthropic-ratelimit-*` headers are surfaced when present.

### 17.6 Observability

- Validation failures are operator-visible
- Structured logging includes issue/session context fields
- Logging sink failures do not crash orchestration
- Token/rate-limit aggregation remains correct across repeated agent updates
- If a human-readable status surface is implemented, it is driven from orchestrator state and does
  not affect correctness
- If humanized event summaries are implemented, they cover key wrapper/agent event classes without
  changing orchestrator behavior

### 17.7 CLI and Host Lifecycle

- CLI accepts a positional workflow path argument (`path-to-WORKFLOW.md`)
- CLI uses `./WORKFLOW.md` when no workflow path argument is provided
- CLI errors on nonexistent explicit workflow path or missing default `./WORKFLOW.md`
- CLI surfaces startup failure cleanly
- CLI exits with success when application starts and shuts down normally
- CLI exits nonzero when startup fails or the host process exits abnormally

### 17.8 Real Integration Profile (RECOMMENDED)

These checks are RECOMMENDED for production readiness and MAY be skipped in CI when credentials,
network access, or external service permissions are unavailable.

- A real tracker smoke test can be run with valid credentials supplied by `LINEAR_API_KEY` or a
  documented local bootstrap mechanism (for example `~/.linear_api_key`).
- Real integration tests SHOULD use isolated test identifiers/workspaces and clean up tracker
  artifacts when practical.
- A skipped real-integration test SHOULD be reported as skipped, not silently treated as passed.
- If a real-integration profile is explicitly enabled in CI or release validation, failures SHOULD
  fail that job.

## 18. Implementation Checklist (Definition of Done)

Use the same validation profiles as Section 17:

- Section 18.1 = `Core Conformance`
- Section 18.2 = `Extension Conformance`
- Section 18.3 = `Real Integration Profile`

### 18.1 REQUIRED for Conformance

- Workflow path selection supports explicit runtime path and cwd default
- `WORKFLOW.md` loader with YAML front matter + prompt body split
- Typed config layer with defaults and `$` resolution
- Dynamic `WORKFLOW.md` watch/reload/re-apply for config and prompt
- Polling orchestrator with single-authority mutable state
- Issue tracker client with candidate fetch + state refresh + terminal fetch
- Workspace manager with sanitized per-issue workspaces
- Workspace lifecycle hooks (`after_create`, `before_run`, `after_run`, `before_remove`)
- Hook timeout config (`hooks.timeout_ms`, default `60000`)
- At least one agent backend client implementing the §10.1 generic
  AgentClient contract. An implementation conforms by supporting any one
  of `codex`, `claude_code`, `openai_compat`, or `anthropic_messages`,
  and MUST document which backends it supports.
- `agent.backend` selector recognized in `WORKFLOW.md` and validated at
  dispatch preflight.
- Backend-specific config block parsed for the selected backend with the
  defaults from §5.3.6.
- Strict prompt rendering with `issue` and `attempt` variables
- Exponential retry queue with continuation retries after normal exit
- Configurable retry backoff cap (`agent.max_retry_backoff_ms`, default 5m)
- Reconciliation that stops runs on terminal/non-active tracker states
- Workspace cleanup for terminal issues (startup sweep + active transition)
- Structured logs with `issue_id`, `issue_identifier`, and `session_id`
- Operator-visible observability (structured logs; OPTIONAL snapshot/status surface)

### 18.2 RECOMMENDED Extensions (Not REQUIRED for Conformance)

- HTTP server extension honors CLI `--port` over `server.port`, uses a safe default bind host, and
  exposes the baseline endpoints/error semantics in Section 13.7 if shipped.
- `linear_graphql` client-side tool extension exposes raw Linear GraphQL
  access through whichever backend's tool-dispatch mechanism is active,
  using configured Symphony auth.
- Operator-control endpoints (§13.7.3): force-retry an issue and browse
  the per-issue workspace from the dashboard so operators don't need
  shell access to inspect or restart a stuck run.
- Live event stream (§13.7.4): a Server-Sent Events feed of agent
  `RuntimeEvent`s so the dashboard can show real-time progress instead
  of polling snapshots.
- First-run diagnostic CLI (`symphony doctor`): runs the dispatch
  preflight (§6.3) plus environment checks — agent backend
  reachability, workspace root writability, hook script syntax, tracker
  auth — and prints a pass/fail checklist. SHOULD exit `0` on full
  green and `1` on any failure.
- Per-issue logs CLI (`symphony logs <identifier>`): tails the
  agent-session logs referenced by the snapshot's `agent_session_logs`
  array without requiring the operator to know the on-disk layout.
- Per-issue cost tracking + daily budget cap: extends `agent_totals`
  with a `cost_usd` field, optional `agent.daily_budget_usd` config
  field, and a hard-stop / warning behavior when the cap is reached.
  Implementations document whether the cap is per-process, per-project,
  or per-tracker.
- WORKFLOW.md JSON schema: a published JSON Schema for the
  `WORKFLOW.md` front matter so editors (VS Code, Zed, etc.) can offer
  autocomplete and diagnostics for the §5.3 schema.
- Multi-workflow process mode: a single `symphony` process drives
  multiple `WORKFLOW.md` files concurrently, sharing the HTTP server
  and process lifecycle but maintaining isolated orchestrator state per
  workflow. Useful when one operator runs Symphony against several
  Linear projects from one host.
- TODO: Persist retry queue and session metadata across process restarts.
- TODO: Make observability settings configurable in workflow front matter without prescribing UI
  implementation details.
- TODO: Add first-class tracker write APIs (comments/state transitions) in the orchestrator instead
  of only via agent tools.
- TODO: Add pluggable issue tracker adapters beyond Linear.

### 18.3 Operational Validation Before Production (RECOMMENDED)

- Run the `Real Integration Profile` from Section 17.8 with valid credentials and network access.
- Verify hook execution and workflow path resolution on the target host OS/shell environment.
- If the OPTIONAL HTTP server is shipped, verify the configured port behavior and loopback/default
  bind expectations on the target environment.

## Appendix A. SSH Worker Extension (OPTIONAL)

This appendix describes a common extension profile in which Symphony keeps one central
orchestrator but executes worker runs on one or more remote hosts over SSH.

Extension config:

- `worker.ssh_hosts` (list of SSH host strings, OPTIONAL)
  - When omitted, work runs locally.
- `worker.max_concurrent_agents_per_host` (positive integer, OPTIONAL)
  - Shared per-host cap applied across configured SSH hosts.

### A.1 Execution Model

- The orchestrator remains the single source of truth for polling, claims, retries, and
  reconciliation.
- `worker.ssh_hosts` provides the candidate SSH destinations for remote execution.
- Each worker run is assigned to one host at a time, and that host becomes part of the run's
  effective execution identity along with the issue workspace.
- `workspace.root` is interpreted on the remote host, not on the orchestrator host.
- For subprocess backends (Codex, Claude Code), the agent runner is
  launched over SSH stdio instead of as a local subprocess, so the
  orchestrator still owns the session lifecycle even though commands
  execute remotely.
- For HTTP backends (OpenAI-compat, Anthropic Messages), the SSH worker
  extension is RECOMMENDED to be no-op or rejected — the agent loop runs
  in-process on the orchestrator host, so SSH offers no benefit beyond
  what `workspace.root` already provides.
- Continuation turns inside one worker lifetime SHOULD stay on the same
  host and workspace.
- A remote host SHOULD satisfy the same basic contract as a local worker
  environment: reachable shell, writable workspace root, the agent
  runner prerequisites for the selected backend (CLI binary or HTTP
  reachability + credentials), and any required auth or repository
  prerequisites.

### A.2 Scheduling Notes

- SSH hosts MAY be treated as a pool for dispatch.
- Implementations MAY prefer the previously used host on retries when that host is still
  available.
- `worker.max_concurrent_agents_per_host` is an OPTIONAL shared per-host cap across configured SSH
  hosts.
- When all SSH hosts are at capacity, dispatch SHOULD wait rather than silently falling back to a
  different execution mode.
- Implementations MAY fail over to another host when the original host is unavailable before work
  has meaningfully started.
- Once a run has already produced side effects, a transparent rerun on another host SHOULD be
  treated as a new attempt, not as invisible failover.

### A.3 Problems to Consider

- Remote environment drift:
  - Each host needs the expected shell environment, agent runner
    prerequisites for the selected backend, auth, and repository
    prerequisites.
- Workspace locality:
  - Workspaces are usually host-local, so moving an issue to a different host is typically a cold
    restart unless shared storage exists.
- Path and command safety:
  - Remote path resolution, shell quoting, and workspace-boundary checks matter more once execution
    crosses a machine boundary.
- Startup and failover semantics:
  - Implementations SHOULD distinguish host-connectivity/startup failures from in-workspace agent
    failures so the same ticket is not accidentally re-executed on multiple hosts.
- Host health and saturation:
  - A dead or overloaded host SHOULD reduce available capacity, not cause duplicate execution or an
    accidental fallback to local work.
- Cleanup and observability:
  - Operators need to know which host owns a run, where its workspace lives, and whether cleanup
    happened on the right machine.
