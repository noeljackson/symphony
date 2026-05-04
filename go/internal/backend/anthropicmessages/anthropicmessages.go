// Package anthropicmessages implements [orchestrator.WorkerRunner] against
// the Anthropic Messages API per SPEC v3 §5.3.6.D.
//
// Mirrors the [openaicompat] backend's lifecycle (workspace → before_run
// → render prompt → POST → emit lifecycle events → after_run) but speaks
// Anthropic's `/v1/messages` request/response shape: `x-api-key` auth,
// required `anthropic-version` header, required `max_tokens`, and a
// content array of typed parts on the response.
package anthropicmessages

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

// AnthropicVersionHeader is the API-version pin sent on every request.
//
// Updating this value is a deliberate per-backend decision; the SPEC keeps
// it at this binary's compile-time choice rather than letting WORKFLOW.md
// override it (the field would couple workflows to vendor-specific
// breaking changes).
const AnthropicVersionHeader = "2023-06-01"

// Runner is a WorkerRunner against `POST <endpoint>/v1/messages`.
type Runner struct {
	cfg       config.AnthropicMessagesConfig
	turnCfg   config.AgentConfig
	workspace *workspace.Manager
	prompt    *prompt.Builder
	http      *http.Client
}

// Config bundles construction inputs.
type Config struct {
	AnthropicMessages config.AnthropicMessagesConfig
	Agent             config.AgentConfig
	Workspace         *workspace.Manager
	Prompt            *prompt.Builder
	HTTPClient        *http.Client // optional; defaults to a 60s-timeout client
}

// New constructs a Runner. The Anthropic API requires `max_tokens` on
// every request, so we surface a config error when it's unset rather
// than silently sending 0 (which Anthropic 400s).
func New(cfg Config) (*Runner, error) {
	if cfg.Workspace == nil {
		return nil, fmt.Errorf("anthropicmessages.New: Workspace is required")
	}
	if cfg.Prompt == nil {
		return nil, fmt.Errorf("anthropicmessages.New: Prompt is required")
	}
	if strings.TrimSpace(cfg.AnthropicMessages.APIKey) == "" {
		return nil, fmt.Errorf("anthropicmessages.New: api_key is required")
	}
	if strings.TrimSpace(cfg.AnthropicMessages.Model) == "" {
		return nil, fmt.Errorf("anthropicmessages.New: model is required")
	}
	if cfg.AnthropicMessages.MaxTokens == 0 {
		return nil, fmt.Errorf("anthropicmessages.New: max_tokens is required (Anthropic 400s without it)")
	}
	if strings.TrimSpace(cfg.AnthropicMessages.Endpoint) == "" {
		cfg.AnthropicMessages.Endpoint = "https://api.anthropic.com"
	}
	cfg.AnthropicMessages.Endpoint = strings.TrimRight(cfg.AnthropicMessages.Endpoint, "/")
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Runner{
		cfg:       cfg.AnthropicMessages,
		turnCfg:   cfg.Agent,
		workspace: cfg.Workspace,
		prompt:    cfg.Prompt,
		http:      hc,
	}, nil
}

// Run satisfies [orchestrator.WorkerRunner]. Lifecycle matches the
// openaicompat backend; only the request/response wire format differs.
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

	emit(ctx, events, orchestrator.AgentEvent{Event: "session_started", Message: i.Identifier})

	usage, err := r.callMessages(ctx, rendered)
	if err != nil {
		return orchestrator.WorkerOutcome{
			Kind:  orchestrator.WorkerOutcomeFailure,
			Error: err.Error(),
		}
	}
	emit(ctx, events, orchestrator.AgentEvent{
		Event:   "turn_completed",
		Message: fmt.Sprintf("usage in=%d out=%d", usage.InputTokens, usage.OutputTokens),
	})
	_ = r.workspace.RunHook(ctx, workspace.HookAfterRun, wsPath, i.Identifier)
	return orchestrator.WorkerOutcome{Kind: orchestrator.WorkerOutcomeSuccess}
}

func emit(ctx context.Context, events chan<- orchestrator.AgentEvent, ev orchestrator.AgentEvent) {
	select {
	case events <- ev:
	case <-ctx.Done():
	}
}

// messagesRequest is the Anthropic Messages API envelope.
type messagesRequest struct {
	Model     string          `json:"model"`
	MaxTokens uint32          `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []messagesEntry `json:"messages"`
}

type messagesEntry struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Role       string             `json:"role"`
	Content    []messagesContent  `json:"content"`
	Model      string             `json:"model"`
	StopReason string             `json:"stop_reason"`
	Usage      messagesUsage      `json:"usage"`
	Error      *messagesErrorBody `json:"error,omitempty"`
}

type messagesContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messagesUsage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
}

type messagesErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (r *Runner) callMessages(ctx context.Context, rendered string) (messagesUsage, error) {
	endpoint := r.cfg.Endpoint + "/v1/messages"
	req := messagesRequest{
		Model:     r.cfg.Model,
		MaxTokens: r.cfg.MaxTokens,
		System:    strings.TrimSpace(r.cfg.System),
		Messages:  []messagesEntry{{Role: "user", Content: rendered}},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return messagesUsage{}, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return messagesUsage{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", r.cfg.APIKey)
	httpReq.Header.Set("anthropic-version", AnthropicVersionHeader)
	resp, err := r.http.Do(httpReq)
	if err != nil {
		return messagesUsage{}, fmt.Errorf("backend_request_failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return messagesUsage{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return messagesUsage{}, fmt.Errorf("anthropic http %d: %s", resp.StatusCode, truncate(string(raw), 256))
	}
	var parsed messagesResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return messagesUsage{}, fmt.Errorf("parse response: %w", err)
	}
	if parsed.Error != nil {
		return messagesUsage{}, fmt.Errorf("anthropic error (%s): %s", parsed.Error.Type, parsed.Error.Message)
	}
	if parsed.Type != "message" {
		return messagesUsage{}, fmt.Errorf("anthropic returned unexpected type: %q", parsed.Type)
	}
	if len(parsed.Content) == 0 {
		return messagesUsage{}, fmt.Errorf("anthropic returned empty content")
	}
	return parsed.Usage, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
