package anthropicmessages_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/backend/anthropicmessages"
	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/prompt"
	"github.com/noeljackson/symphony/go/internal/workspace"
)

func makeWorkspace(t *testing.T) *workspace.Manager {
	t.Helper()
	m, err := workspace.New(t.TempDir(), workspace.Hooks{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	return m
}

func makePrompt(t *testing.T, body string) *prompt.Builder {
	t.Helper()
	b, err := prompt.New(body)
	if err != nil {
		t.Fatalf("prompt.New: %v", err)
	}
	return b
}

func mkIssue(ident string) issue.Issue {
	return issue.Issue{ID: "id-" + ident, Identifier: ident, Title: "Title for " + ident, State: "Todo"}
}

func drainEvents(ch <-chan orchestrator.AgentEvent, timeout time.Duration) []orchestrator.AgentEvent {
	var out []orchestrator.AgentEvent
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline.C:
			return out
		}
	}
}

type capturedRequest struct {
	APIKey  string
	Version string
	Path    string
	Body    map[string]any
}

func TestRunCallsBackendAndEmitsLifecycleEvents(t *testing.T) {
	captured := make(chan capturedRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		captured <- capturedRequest{
			APIKey:  r.Header.Get("x-api-key"),
			Version: r.Header.Get("anthropic-version"),
			Path:    r.URL.Path,
			Body:    body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_x",
			"type":"message",
			"role":"assistant",
			"content":[{"type":"text","text":"ok"}],
			"model":"claude-opus-4-7",
			"stop_reason":"end_turn",
			"usage":{"input_tokens":12,"output_tokens":7}
		}`))
	}))
	defer srv.Close()

	r, err := anthropicmessages.New(anthropicmessages.Config{
		AnthropicMessages: config.AnthropicMessagesConfig{
			Endpoint: srv.URL, APIKey: "test-key", Model: "claude-opus-4-7", MaxTokens: 4096,
			System: "You are a senior engineer.",
		},
		Agent:     config.AgentConfig{MaxTurns: 1},
		Workspace: makeWorkspace(t),
		Prompt:    makePrompt(t, "Issue {{ issue.identifier }}: {{ issue.title }}"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events := make(chan orchestrator.AgentEvent, 8)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeSuccess {
		t.Fatalf("outcome: got %v want Success (err=%q)", outcome.Kind, outcome.Error)
	}
	got := drainEvents(events, 100*time.Millisecond)
	if len(got) < 2 || got[0].Event != "session_started" || got[len(got)-1].Event != "turn_completed" {
		t.Fatalf("events: got %+v", got)
	}
	if !strings.Contains(got[len(got)-1].Message, "in=12") {
		t.Fatalf("turn_completed missing usage: %q", got[len(got)-1].Message)
	}

	req := <-captured
	if req.APIKey != "test-key" {
		t.Fatalf("x-api-key: got %q want test-key", req.APIKey)
	}
	if req.Version != anthropicmessages.AnthropicVersionHeader {
		t.Fatalf("anthropic-version: got %q want %q", req.Version, anthropicmessages.AnthropicVersionHeader)
	}
	if req.Path != "/v1/messages" {
		t.Fatalf("path: got %q want /v1/messages", req.Path)
	}
	if req.Body["model"] != "claude-opus-4-7" {
		t.Fatalf("model: got %v", req.Body["model"])
	}
	if int(req.Body["max_tokens"].(float64)) != 4096 {
		t.Fatalf("max_tokens: got %v", req.Body["max_tokens"])
	}
	if !strings.Contains(req.Body["system"].(string), "senior engineer") {
		t.Fatalf("system: got %v", req.Body["system"])
	}
	msgs := req.Body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages: got %d want 1 (anthropic carries system separately)", len(msgs))
	}
	user := msgs[0].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("role: got %v want user", user["role"])
	}
	if !strings.Contains(user["content"].(string), "Issue MT-1: Title for MT-1") {
		t.Fatalf("user content: %v", user["content"])
	}
}

func TestRunSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()
	r, _ := anthropicmessages.New(anthropicmessages.Config{
		AnthropicMessages: config.AnthropicMessagesConfig{Endpoint: srv.URL, APIKey: "k", Model: "m", MaxTokens: 1024},
		Agent:             config.AgentConfig{},
		Workspace:         makeWorkspace(t),
		Prompt:            makePrompt(t, "x"),
	})
	events := make(chan orchestrator.AgentEvent, 4)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure {
		t.Fatalf("outcome: got %v want Failure", outcome.Kind)
	}
	if !strings.Contains(outcome.Error, "401") {
		t.Fatalf("error: got %q want substring 401", outcome.Error)
	}
}

func TestRunSurfacesAnthropicErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"max_tokens too high"}}`))
	}))
	defer srv.Close()
	r, _ := anthropicmessages.New(anthropicmessages.Config{
		AnthropicMessages: config.AnthropicMessagesConfig{Endpoint: srv.URL, APIKey: "k", Model: "m", MaxTokens: 4096},
		Agent:             config.AgentConfig{},
		Workspace:         makeWorkspace(t),
		Prompt:            makePrompt(t, "x"),
	})
	events := make(chan orchestrator.AgentEvent, 4)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure {
		t.Fatalf("outcome: got %v want Failure", outcome.Kind)
	}
	if !strings.Contains(outcome.Error, "max_tokens too high") {
		t.Fatalf("error: got %q", outcome.Error)
	}
}

func TestRunFailsWhenContentEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","content":[],"model":"x","usage":{"input_tokens":1,"output_tokens":0}}`))
	}))
	defer srv.Close()
	r, _ := anthropicmessages.New(anthropicmessages.Config{
		AnthropicMessages: config.AnthropicMessagesConfig{Endpoint: srv.URL, APIKey: "k", Model: "m", MaxTokens: 1024},
		Agent:             config.AgentConfig{},
		Workspace:         makeWorkspace(t),
		Prompt:            makePrompt(t, "x"),
	})
	events := make(chan orchestrator.AgentEvent, 4)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure || !strings.Contains(outcome.Error, "empty content") {
		t.Fatalf("outcome: got %+v want Failure with 'empty content'", outcome)
	}
}

func TestNewRejectsMissingFields(t *testing.T) {
	cases := []anthropicmessages.Config{
		// missing api_key
		{AnthropicMessages: config.AnthropicMessagesConfig{Model: "m", MaxTokens: 1024}, Workspace: makeWorkspace(t), Prompt: makePrompt(t, "x")},
		// missing model
		{AnthropicMessages: config.AnthropicMessagesConfig{APIKey: "k", MaxTokens: 1024}, Workspace: makeWorkspace(t), Prompt: makePrompt(t, "x")},
		// missing max_tokens
		{AnthropicMessages: config.AnthropicMessagesConfig{APIKey: "k", Model: "m"}, Workspace: makeWorkspace(t), Prompt: makePrompt(t, "x")},
		// missing workspace
		{AnthropicMessages: config.AnthropicMessagesConfig{APIKey: "k", Model: "m", MaxTokens: 1024}, Prompt: makePrompt(t, "x")},
		// missing prompt
		{AnthropicMessages: config.AnthropicMessagesConfig{APIKey: "k", Model: "m", MaxTokens: 1024}, Workspace: makeWorkspace(t)},
	}
	for i, c := range cases {
		if _, err := anthropicmessages.New(c); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}
