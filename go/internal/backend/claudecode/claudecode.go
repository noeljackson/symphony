// Package claudecode implements [orchestrator.WorkerRunner] against the
// Claude Code CLI's stream-json mode per SPEC v3 §5.3.6.B.
//
// Protocol: spawn the configured `claude` command (default `claude --print
// --output-format stream-json --input-format stream-json --verbose`), wait
// for the `system/init` event so we know the CLI is ready, send a single
// user message to stdin (one JSON line, then close stdin), then read
// newline-delimited JSON events from stdout until a `result` event lands.
//
// One non-streaming turn per attempt. The orchestrator's retry loop is
// responsible for re-dispatching when the issue isn't done; this backend
// runs exactly one prompt → response cycle and exits.
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/prompt"
	"github.com/noeljackson/symphony/go/internal/workspace"
)

// Runner is a [orchestrator.WorkerRunner] for the Claude Code stdio CLI.
type Runner struct {
	cfg       config.ClaudeCodeConfig
	turnCfg   config.AgentConfig
	workspace *workspace.Manager
	prompt    *prompt.Builder
}

// Config bundles construction inputs.
type Config struct {
	ClaudeCode config.ClaudeCodeConfig
	Agent      config.AgentConfig
	Workspace  *workspace.Manager
	Prompt     *prompt.Builder
}

// New constructs a Runner. The claude command is required; everything else
// has SPEC §5.3.6.B defaults.
func New(cfg Config) (*Runner, error) {
	if cfg.Workspace == nil {
		return nil, fmt.Errorf("claudecode.New: Workspace is required")
	}
	if cfg.Prompt == nil {
		return nil, fmt.Errorf("claudecode.New: Prompt is required")
	}
	if strings.TrimSpace(cfg.ClaudeCode.Command) == "" {
		return nil, fmt.Errorf("claudecode.New: command is required")
	}
	return &Runner{
		cfg:       cfg.ClaudeCode,
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

	turnTimeout := r.turnTimeout()
	turnCtx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()

	outcome := r.runOneTurn(turnCtx, wsPath, rendered, events)

	// after_run is best-effort and never overrides the turn outcome.
	_ = r.workspace.RunHook(ctx, workspace.HookAfterRun, wsPath, i.Identifier)
	return outcome
}

func (r *Runner) turnTimeout() time.Duration {
	ms := r.cfg.TurnTimeoutMS
	if ms == 0 {
		ms = 3_600_000
	}
	return time.Duration(ms) * time.Millisecond
}

func (r *Runner) runOneTurn(
	ctx context.Context,
	wsPath, rendered string,
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
		// `command not found`-style errors come through Start.
		if errors.Is(err, exec.ErrNotFound) {
			return failure("agent_runner_not_found", err)
		}
		return failure("startup_failed", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // tolerate large model outputs

	if err := awaitInit(scanner); err != nil {
		_ = cmd.Wait()
		return failure("startup_failed", err)
	}
	emit(ctx, events, orchestrator.AgentEvent{Event: "session_started", Message: ""})

	if err := writeUserMessage(stdin, rendered); err != nil {
		return failure("stdin_write_failed", err)
	}
	_ = stdin.Close() // signals end-of-input to the CLI

	usage, err := readUntilResult(ctx, scanner, events)
	waitErr := cmd.Wait()
	if err != nil {
		// readUntilResult error includes any stream / decode issue.
		return failure("turn_failed", err)
	}
	if waitErr != nil {
		// Process exited non-zero AFTER emitting result; surface the wait error
		// so the orchestrator can decide whether to retry.
		return failure("agent_exited_nonzero", waitErr)
	}
	emit(ctx, events, orchestrator.AgentEvent{
		Event:   "turn_completed",
		Message: fmt.Sprintf("usage in=%d out=%d total=%d", usage.InputTokens, usage.OutputTokens, usage.TotalTokens),
	})
	return orchestrator.WorkerOutcome{Kind: orchestrator.WorkerOutcomeSuccess}
}

func awaitInit(s *bufio.Scanner) error {
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var env envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return fmt.Errorf("decode init: %w", err)
		}
		if env.Type == "system" && env.Subtype == "init" {
			return nil
		}
		// Anything else before init is unexpected — surface it.
		return fmt.Errorf("expected system/init, got type=%q subtype=%q", env.Type, env.Subtype)
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("read init: %w", err)
	}
	return errors.New("agent stream closed before system/init")
}

func writeUserMessage(w io.WriteCloser, rendered string) error {
	body := userMessage{
		Type: "user",
		Message: userInner{
			Role: "user",
			Content: []userContent{
				{Type: "text", Text: rendered},
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal user message: %w", err)
	}
	raw = append(raw, '\n')
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("write user message: %w", err)
	}
	return nil
}

func readUntilResult(ctx context.Context, s *bufio.Scanner, events chan<- orchestrator.AgentEvent) (resultUsage, error) {
	var (
		usage resultUsage
		done  bool
	)
	for s.Scan() {
		if ctx.Err() != nil {
			return usage, ctx.Err()
		}
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var env envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			// Best-effort: the CLI sometimes emits non-JSON lines under
			// --verbose. Skip them rather than aborting the whole turn.
			continue
		}
		switch env.Type {
		case "assistant":
			emit(ctx, events, orchestrator.AgentEvent{
				Event:   "assistant_message",
				Message: extractAssistantText(env),
			})
		case "tool_use":
			emit(ctx, events, orchestrator.AgentEvent{Event: "tool_use", Message: env.ToolName})
		case "tool_result":
			emit(ctx, events, orchestrator.AgentEvent{Event: "tool_result", Message: env.ToolName})
		case "result":
			usage = env.Usage
			if env.Subtype == "error" {
				return usage, fmt.Errorf("result/error: %s", env.Message)
			}
			done = true
		}
		if done {
			return usage, nil
		}
	}
	if err := s.Err(); err != nil {
		return usage, fmt.Errorf("read stream: %w", err)
	}
	if !done {
		return usage, errors.New("agent stream closed before result")
	}
	return usage, nil
}

func extractAssistantText(env envelope) string {
	for _, m := range env.MessageContent {
		if m.Type == "text" && m.Text != "" {
			return m.Text
		}
	}
	return ""
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

// envelope is the shared shape for stream-json events.
type envelope struct {
	Type           string                   `json:"type"`
	Subtype        string                   `json:"subtype"`
	SessionID      string                   `json:"session_id,omitempty"`
	Model          string                   `json:"model,omitempty"`
	ToolName       string                   `json:"tool_name,omitempty"`
	Message        string                   `json:"message,omitempty"`
	Usage          resultUsage              `json:"usage"`
	MessageContent []envelopeMessageContent `json:"content,omitempty"`
}

type envelopeMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type resultUsage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	TotalTokens  uint64 `json:"total_tokens"`
}

type userMessage struct {
	Type    string    `json:"type"`
	Message userInner `json:"message"`
}

type userInner struct {
	Role    string        `json:"role"`
	Content []userContent `json:"content"`
}

type userContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
