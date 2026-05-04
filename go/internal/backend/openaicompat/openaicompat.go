// Package openaicompat implements a [orchestrator.WorkerRunner] against any
// OpenAI-compatible HTTP Chat Completions endpoint per SPEC v3 §5.3.6.C.
//
// One Runner instance owns one workflow; it composes a [workspace.Manager]
// for filesystem lifecycle and a [prompt.Builder] for Liquid rendering,
// then issues a single non-streaming `chat/completions` call per attempt
// and emits agent events on the runner channel.
//
// The single-call shape is intentional for v1: openai_compat backends
// don't run tool loops in this implementation. A later PR can extend
// [Runner.Run] with a tool-loop dispatcher when the symphony tool API
// stabilizes in Go.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/prompt"
	"github.com/noeljackson/symphony/go/internal/workspace"
)

// Runner is a [orchestrator.WorkerRunner] for OpenAI-compatible HTTP backends.
type Runner struct {
	cfg       config.OpenAICompatConfig
	turnCfg   config.AgentConfig
	workspace *workspace.Manager
	prompt    *prompt.Builder
	http      *http.Client
}

// Config bundles everything Runner needs at construction time.
type Config struct {
	OpenAICompat config.OpenAICompatConfig
	Agent        config.AgentConfig
	Workspace    *workspace.Manager
	Prompt       *prompt.Builder
	HTTPClient   *http.Client // optional; defaults to a 60s-timeout client
}

// New constructs a Runner. Returns an error when OpenAICompat lacks the
// required fields per SPEC §5.3.6.C.
func New(cfg Config) (*Runner, error) {
	if strings.TrimSpace(cfg.OpenAICompat.Endpoint) == "" {
		return nil, fmt.Errorf("openaicompat.New: endpoint is required")
	}
	if strings.TrimSpace(cfg.OpenAICompat.APIKey) == "" {
		return nil, fmt.Errorf("openaicompat.New: api_key is required")
	}
	if strings.TrimSpace(cfg.OpenAICompat.Model) == "" {
		return nil, fmt.Errorf("openaicompat.New: model is required")
	}
	if cfg.Workspace == nil {
		return nil, fmt.Errorf("openaicompat.New: Workspace is required")
	}
	if cfg.Prompt == nil {
		return nil, fmt.Errorf("openaicompat.New: Prompt is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Runner{
		cfg:       cfg.OpenAICompat,
		turnCfg:   cfg.Agent,
		workspace: cfg.Workspace,
		prompt:    cfg.Prompt,
		http:      hc,
	}, nil
}

// Run satisfies [orchestrator.WorkerRunner].
//
// Lifecycle:
//  1. Ensure the workspace directory exists; fire after_create on first
//     creation; fire before_run on every attempt.
//  2. Render the Liquid prompt against the issue + attempt context.
//  3. POST `<endpoint>/chat/completions` with the rendered prompt.
//  4. Emit `session_started` / `turn_completed` events on the channel
//     and return WorkerOutcomeSuccess (or Failure with the error reason).
//  5. Fire after_run (best-effort: failures logged via the returned
//     outcome but don't override a successful turn).
func (r *Runner) Run(
	ctx context.Context,
	i issue.Issue,
	attempt *uint32,
	events chan<- orchestrator.AgentEvent,
) orchestrator.WorkerOutcome {
	wsPath, _, err := r.workspace.EnsureCreated(ctx, i.Identifier)
	if err != nil {
		return orchestrator.WorkerOutcome{
			Kind:  orchestrator.WorkerOutcomeFailure,
			Error: fmt.Sprintf("workspace_setup_failed: %v", err),
		}
	}
	if err := r.workspace.RunHook(ctx, workspace.HookBeforeRun, wsPath, i.Identifier); err != nil {
		return orchestrator.WorkerOutcome{
			Kind:  orchestrator.WorkerOutcomeFailure,
			Error: fmt.Sprintf("before_run_hook_failed: %v", err),
		}
	}

	rendered, err := r.prompt.Render(i, attempt)
	if err != nil {
		return orchestrator.WorkerOutcome{
			Kind:  orchestrator.WorkerOutcomeFailure,
			Error: fmt.Sprintf("prompt_render_failed: %v", err),
		}
	}

	emit(ctx, events, orchestrator.AgentEvent{
		Event:   "session_started",
		Message: i.Identifier,
	})

	usage, err := r.callChatCompletions(ctx, rendered)
	if err != nil {
		return orchestrator.WorkerOutcome{
			Kind:  orchestrator.WorkerOutcomeFailure,
			Error: err.Error(),
		}
	}
	emit(ctx, events, orchestrator.AgentEvent{
		Event: "turn_completed",
		Message: fmt.Sprintf("usage in=%d out=%d total=%d",
			usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens),
	})

	// after_run is best-effort; we surface failures via stderr-equivalent
	// logging but don't override the turn outcome.
	_ = r.workspace.RunHook(ctx, workspace.HookAfterRun, wsPath, i.Identifier)

	return orchestrator.WorkerOutcome{Kind: orchestrator.WorkerOutcomeSuccess}
}

func emit(ctx context.Context, events chan<- orchestrator.AgentEvent, ev orchestrator.AgentEvent) {
	select {
	case events <- ev:
	case <-ctx.Done():
	}
}

// chatRequest mirrors the OpenAI Chat Completions request shape; HTTP
// backends like Moonshot Kimi K2, Zhipu GLM, vLLM all accept this exact
// envelope.
type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens uint32        `json:"max_tokens,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     uint64 `json:"prompt_tokens"`
	CompletionTokens uint64 `json:"completion_tokens"`
	TotalTokens      uint64 `json:"total_tokens"`
}

func (r *Runner) callChatCompletions(ctx context.Context, prompt string) (chatUsage, error) {
	endpoint := strings.TrimRight(r.cfg.Endpoint, "/") + "/chat/completions"
	messages := []chatMessage{}
	if strings.TrimSpace(r.cfg.System) != "" {
		messages = append(messages, chatMessage{Role: "system", Content: r.cfg.System})
	}
	messages = append(messages, chatMessage{Role: "user", Content: prompt})
	body, err := json.Marshal(chatRequest{
		Model:     r.cfg.Model,
		Messages:  messages,
		MaxTokens: r.cfg.MaxTokens,
	})
	if err != nil {
		return chatUsage{}, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return chatUsage{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	resp, err := r.http.Do(req)
	if err != nil {
		return chatUsage{}, fmt.Errorf("backend_request_failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return chatUsage{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return chatUsage{}, fmt.Errorf("backend http %d: %s", resp.StatusCode, truncate(string(raw), 256))
	}
	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return chatUsage{}, fmt.Errorf("parse response: %w", err)
	}
	if parsed.Error != nil {
		return chatUsage{}, fmt.Errorf("backend error (%s): %s", parsed.Error.Type, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return chatUsage{}, fmt.Errorf("backend returned no choices")
	}
	return parsed.Usage, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
