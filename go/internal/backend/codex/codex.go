// Package codex implements [orchestrator.WorkerRunner] against the Codex
// app-server's JSON-RPC stdio protocol per SPEC v3 §5.3.6.A / §10.A.
//
// Protocol: spawn `codex app-server` (configurable), then drive
// `initialize` → `initialized` → `thread/start` → `turn/start` plus a
// streaming loop of notifications until `turn/completed`. One turn per
// attempt; the orchestrator's retry loop handles re-dispatch.
//
// Notifications surfaced to the orchestrator's event channel:
//   - `session_started` — emitted once thread/start succeeds.
//   - `assistant_message` — `item/assistantMessage` events.
//   - `tool_use` / `tool_result` — `item/tool/*` events with the tool name.
//   - `turn_completed` — final usage line from the most recent
//     `thread/tokenUsage/updated` payload.
//
// Tool dispatch (`item/tool/call` requiring a client response) is reserved
// for a follow-up PR; v1 surfaces tool-call events but doesn't reply.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/prompt"
	"github.com/noeljackson/symphony/go/internal/workspace"
)

// Runner is a [orchestrator.WorkerRunner] for the Codex app-server.
type Runner struct {
	cfg       config.CodexConfig
	turnCfg   config.AgentConfig
	workspace *workspace.Manager
	prompt    *prompt.Builder
}

// Config bundles construction inputs.
type Config struct {
	Codex     config.CodexConfig
	Agent     config.AgentConfig
	Workspace *workspace.Manager
	Prompt    *prompt.Builder
}

// New constructs a Runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Workspace == nil {
		return nil, fmt.Errorf("codex.New: Workspace is required")
	}
	if cfg.Prompt == nil {
		return nil, fmt.Errorf("codex.New: Prompt is required")
	}
	if strings.TrimSpace(cfg.Codex.Command) == "" {
		return nil, fmt.Errorf("codex.New: command is required")
	}
	return &Runner{
		cfg:       cfg.Codex,
		turnCfg:   cfg.Agent,
		workspace: cfg.Workspace,
		prompt:    cfg.Prompt,
	}, nil
}

// Run satisfies [orchestrator.WorkerRunner].
func (r *Runner) Run(
	ctx context.Context,
	i issue.Issue,
	attempt *uint32,
	events chan<- orchestrator.AgentEvent,
) orchestrator.WorkerOutcome {
	wsPath, _, err := r.workspace.EnsureCreated(ctx, i.Identifier)
	if err != nil {
		return failure("workspace_setup_failed", err)
	}
	if err := r.workspace.RunHook(ctx, workspace.HookBeforeRun, wsPath, i.Identifier); err != nil {
		return failure("before_run_hook_failed", err)
	}
	rendered, err := r.prompt.Render(i, attempt)
	if err != nil {
		return failure("prompt_render_failed", err)
	}

	turnTimeout := time.Duration(or(r.cfg.TurnTimeoutMS, 3_600_000)) * time.Millisecond
	turnCtx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()

	outcome := r.runOneTurn(turnCtx, wsPath, rendered, i, events)
	_ = r.workspace.RunHook(ctx, workspace.HookAfterRun, wsPath, i.Identifier)
	return outcome
}

func or(v, fallback uint64) uint64 {
	if v == 0 {
		return fallback
	}
	return v
}

func (r *Runner) runOneTurn(
	ctx context.Context,
	wsPath, rendered string,
	i issue.Issue,
	events chan<- orchestrator.AgentEvent,
) orchestrator.WorkerOutcome {
	cmd := exec.CommandContext(ctx, "bash", "-lc", r.cfg.Command)
	cmd.Dir = wsPath

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return failure("startup_failed", fmt.Errorf("stdin pipe: %w", err))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failure("startup_failed", fmt.Errorf("stdout pipe: %w", err))
	}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return failure("agent_runner_not_found", err)
		}
		return failure("startup_failed", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	c := newClient(stdin, stdout)
	defer c.close()

	if err := c.start(ctx); err != nil {
		_ = cmd.Wait()
		return failure("startup_failed", err)
	}
	threadID, err := c.startThread(ctx, wsPath, r.cfg)
	if err != nil {
		_ = cmd.Wait()
		return failure("startup_failed", err)
	}
	emit(ctx, events, orchestrator.AgentEvent{Event: "session_started", Message: i.Identifier})

	usage, turnErr := c.runTurn(ctx, threadID, rendered, wsPath, r.cfg, events)
	_ = stdin.Close()
	waitErr := cmd.Wait()
	if turnErr != nil {
		return failure("turn_failed", turnErr)
	}
	if waitErr != nil {
		return failure("agent_exited_nonzero", waitErr)
	}
	emit(ctx, events, orchestrator.AgentEvent{
		Event:   "turn_completed",
		Message: fmt.Sprintf("usage in=%d out=%d total=%d", usage.InputTokens, usage.OutputTokens, usage.TotalTokens),
	})
	return orchestrator.WorkerOutcome{Kind: orchestrator.WorkerOutcomeSuccess}
}

func emit(ctx context.Context, events chan<- orchestrator.AgentEvent, ev orchestrator.AgentEvent) {
	select {
	case events <- ev:
	case <-ctx.Done():
	}
}

func failure(code string, err error) orchestrator.WorkerOutcome {
	return orchestrator.WorkerOutcome{
		Kind:  orchestrator.WorkerOutcomeFailure,
		Error: fmt.Sprintf("%s: %v", code, err),
	}
}

// client owns the JSON-RPC stdio channel. Single-threaded use: only the
// orchestrator goroutine calls these methods.
type client struct {
	stdin  io.Writer
	scan   *bufio.Scanner
	nextID atomic.Int64
	mu     sync.Mutex // guards stdin writes
}

func newClient(stdin io.Writer, stdout io.Reader) *client {
	s := bufio.NewScanner(stdout)
	s.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &client{stdin: stdin, scan: s}
}

func (c *client) close() {}

// start runs the SPEC §10.2 handshake: initialize → initialized.
func (c *client) start(ctx context.Context) error {
	id := c.send("initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "symphony-orchestrator",
			"title":   "Symphony Orchestrator",
			"version": "go",
		},
	})
	if _, err := c.awaitResponse(ctx, id); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	c.notify("initialized", map[string]any{})
	return nil
}

// startThread issues thread/start and returns the assigned thread id.
func (c *client) startThread(ctx context.Context, cwd string, cfg config.CodexConfig) (string, error) {
	params := map[string]any{
		"cwd": cwd,
	}
	if cfg.ApprovalPolicy != nil {
		params["approvalPolicy"] = cfg.ApprovalPolicy
	}
	if cfg.ThreadSandbox != nil {
		params["sandbox"] = cfg.ThreadSandbox
	}
	id := c.send("thread/start", params)
	resp, err := c.awaitResponse(ctx, id)
	if err != nil {
		return "", fmt.Errorf("thread/start: %w", err)
	}
	threadID := resp.threadID()
	if threadID == "" {
		return "", fmt.Errorf("thread/start response missing thread.id")
	}
	return threadID, nil
}

// turnUsage carries the absolute thread-total usage SPEC §13.5 prefers.
type turnUsage struct {
	InputTokens  uint64
	OutputTokens uint64
	TotalTokens  uint64
}

// runTurn issues turn/start and reads notifications until turn/completed
// or turn/failed lands. Tool calls are surfaced as events but not yet
// dispatched (deferred to a follow-up PR).
func (c *client) runTurn(
	ctx context.Context,
	threadID, prompt, cwd string,
	cfg config.CodexConfig,
	events chan<- orchestrator.AgentEvent,
) (turnUsage, error) {
	var usage turnUsage
	params := map[string]any{
		"threadId": threadID,
		"input":    []map[string]any{{"type": "text", "text": prompt}},
		"cwd":      cwd,
	}
	if cfg.ApprovalPolicy != nil {
		params["approvalPolicy"] = cfg.ApprovalPolicy
	}
	if cfg.TurnSandboxPolicy != nil {
		params["sandboxPolicy"] = cfg.TurnSandboxPolicy
	}
	startID := c.send("turn/start", params)
	if _, err := c.awaitResponse(ctx, startID); err != nil {
		return usage, fmt.Errorf("turn/start: %w", err)
	}

	for {
		if ctx.Err() != nil {
			return usage, ctx.Err()
		}
		msg, err := c.readMessage()
		if err != nil {
			return usage, err
		}
		if msg.ID != 0 {
			// Stray response with no waiter — ignore.
			continue
		}
		switch msg.Method {
		case "thread/tokenUsage/updated":
			usage = msg.usage()
		case "item/assistantMessage":
			emit(ctx, events, orchestrator.AgentEvent{
				Event:   "assistant_message",
				Message: msg.assistantText(),
			})
		case "item/tool/call":
			emit(ctx, events, orchestrator.AgentEvent{Event: "tool_use", Message: msg.toolName()})
		case "item/tool/result":
			emit(ctx, events, orchestrator.AgentEvent{Event: "tool_result", Message: msg.toolName()})
		case "turn/completed":
			return usage, nil
		case "turn/failed":
			return usage, fmt.Errorf("turn/failed: %s", msg.failureMessage())
		case "turn/cancelled":
			return usage, fmt.Errorf("turn/cancelled")
		}
	}
}

// rpcMessage is the parsed view of one line on stdout.
type rpcMessage struct {
	ID     int64           `json:"id"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (m *rpcMessage) threadID() string {
	if len(m.Result) == 0 {
		return ""
	}
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	_ = json.Unmarshal(m.Result, &r)
	return r.Thread.ID
}

func (m *rpcMessage) usage() turnUsage {
	if len(m.Params) == 0 {
		return turnUsage{}
	}
	// Codex emits either snake_case (input_tokens) or camelCase
	// (inputTokens) depending on app-server version. Try both and let
	// whichever wins fill the struct.
	var snake struct {
		InputTokens  uint64 `json:"input_tokens"`
		OutputTokens uint64 `json:"output_tokens"`
		TotalTokens  uint64 `json:"total_tokens"`
	}
	var camel struct {
		InputTokens  uint64 `json:"inputTokens"`
		OutputTokens uint64 `json:"outputTokens"`
		TotalTokens  uint64 `json:"totalTokens"`
	}
	_ = json.Unmarshal(m.Params, &snake)
	_ = json.Unmarshal(m.Params, &camel)
	pick := func(a, b uint64) uint64 {
		if a > 0 {
			return a
		}
		return b
	}
	return turnUsage{
		InputTokens:  pick(snake.InputTokens, camel.InputTokens),
		OutputTokens: pick(snake.OutputTokens, camel.OutputTokens),
		TotalTokens:  pick(snake.TotalTokens, camel.TotalTokens),
	}
}

func (m *rpcMessage) assistantText() string {
	if len(m.Params) == 0 {
		return ""
	}
	var p struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(m.Params, &p)
	for _, c := range p.Content {
		if c.Type == "text" && c.Text != "" {
			return c.Text
		}
	}
	return p.Text
}

func (m *rpcMessage) toolName() string {
	if len(m.Params) == 0 {
		return ""
	}
	var p struct {
		Name     string `json:"name"`
		ToolName string `json:"toolName"`
	}
	_ = json.Unmarshal(m.Params, &p)
	if p.Name != "" {
		return p.Name
	}
	return p.ToolName
}

func (m *rpcMessage) failureMessage() string {
	if len(m.Params) == 0 {
		return ""
	}
	var p struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(m.Params, &p)
	return p.Message
}

// send writes a request envelope and returns the assigned id.
func (c *client) send(method string, params map[string]any) int64 {
	id := c.nextID.Add(1)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	c.writeLine(body)
	return id
}

// notify writes a notification envelope (no id, no response expected).
func (c *client) notify(method string, params map[string]any) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	c.writeLine(body)
}

func (c *client) writeLine(body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.stdin.Write(append(body, '\n'))
}

// awaitResponse reads stdout until it sees a response with the given id.
// Notifications encountered along the way are dropped — the caller
// (runTurn) will pick them up after the response lands.
//
// This single-threaded reader works because the codex app-server is
// strictly request-response: handshake responses arrive before any turn
// notifications start streaming.
func (c *client) awaitResponse(ctx context.Context, id int64) (*rpcMessage, error) {
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg, err := c.readMessage()
		if err != nil {
			return nil, err
		}
		if msg.ID == id {
			if msg.Error != nil {
				return nil, fmt.Errorf("rpc error %d: %s", msg.Error.Code, msg.Error.Message)
			}
			return msg, nil
		}
		// Notifications during handshake are unusual but not fatal — drop them.
	}
}

func (c *client) readMessage() (*rpcMessage, error) {
	for c.scan.Scan() {
		line := strings.TrimSpace(c.scan.Text())
		if line == "" {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			// Best-effort: skip non-JSON noise.
			continue
		}
		return &msg, nil
	}
	if err := c.scan.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	return nil, errors.New("agent stream closed")
}
