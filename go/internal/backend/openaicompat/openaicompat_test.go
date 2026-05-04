package openaicompat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/backend/openaicompat"
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
	return issue.Issue{
		ID:         "id-" + ident,
		Identifier: ident,
		Title:      "Title for " + ident,
		State:      "Todo",
	}
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

func TestRunCallsBackendAndEmitsLifecycleEvents(t *testing.T) {
	var captured chatRequestCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		captured.Auth = r.Header.Get("Authorization")
		_ = json.Unmarshal(raw, &captured.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"x",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}
		}`))
	}))
	defer srv.Close()

	r, err := openaicompat.New(openaicompat.Config{
		OpenAICompat: config.OpenAICompatConfig{
			Endpoint: srv.URL, APIKey: "test-key", Model: "kimi-k2", MaxTokens: 256,
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
	if len(got) < 2 {
		t.Fatalf("events: got %d want >=2 (started+completed): %+v", len(got), got)
	}
	if got[0].Event != "session_started" || got[len(got)-1].Event != "turn_completed" {
		t.Fatalf("event order: got %+v", got)
	}
	if !strings.Contains(got[len(got)-1].Message, "in=12") {
		t.Fatalf("turn_completed message missing usage: %q", got[len(got)-1].Message)
	}

	if captured.Auth != "Bearer test-key" {
		t.Fatalf("auth: got %q want %q", captured.Auth, "Bearer test-key")
	}
	if captured.Body["model"] != "kimi-k2" {
		t.Fatalf("model: got %v want kimi-k2", captured.Body["model"])
	}
	msgs, ok := captured.Body["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages: got %+v want [system, user]", captured.Body["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(first["content"].(string), "senior engineer") {
		t.Fatalf("system message: got %+v", first)
	}
	user := msgs[1].(map[string]any)
	if !strings.Contains(user["content"].(string), "Issue MT-1: Title for MT-1") {
		t.Fatalf("user message missing rendered prompt: %+v", user)
	}
}

func TestRunSurfacesBackendHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"auth"}}`))
	}))
	defer srv.Close()

	r, _ := openaicompat.New(openaicompat.Config{
		OpenAICompat: config.OpenAICompatConfig{
			Endpoint: srv.URL, APIKey: "k", Model: "m",
		},
		Agent:     config.AgentConfig{},
		Workspace: makeWorkspace(t),
		Prompt:    makePrompt(t, "x"),
	})
	events := make(chan orchestrator.AgentEvent, 8)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure {
		t.Fatalf("outcome: got %v want Failure", outcome.Kind)
	}
	if !strings.Contains(outcome.Error, "401") {
		t.Fatalf("error: got %q want substring 401", outcome.Error)
	}
}

func TestRunSurfacesBackendErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"context length exceeded","type":"invalid_request"}}`))
	}))
	defer srv.Close()

	r, _ := openaicompat.New(openaicompat.Config{
		OpenAICompat: config.OpenAICompatConfig{Endpoint: srv.URL, APIKey: "k", Model: "m"},
		Agent:        config.AgentConfig{},
		Workspace:    makeWorkspace(t),
		Prompt:       makePrompt(t, "x"),
	})
	events := make(chan orchestrator.AgentEvent, 8)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure {
		t.Fatalf("outcome: got %v want Failure", outcome.Kind)
	}
	if !strings.Contains(outcome.Error, "context length exceeded") {
		t.Fatalf("error: got %q", outcome.Error)
	}
}

func TestRunFailsWhenBackendReturnsNoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`))
	}))
	defer srv.Close()

	r, _ := openaicompat.New(openaicompat.Config{
		OpenAICompat: config.OpenAICompatConfig{Endpoint: srv.URL, APIKey: "k", Model: "m"},
		Agent:        config.AgentConfig{},
		Workspace:    makeWorkspace(t),
		Prompt:       makePrompt(t, "x"),
	})
	events := make(chan orchestrator.AgentEvent, 8)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure ||
		!strings.Contains(outcome.Error, "no choices") {
		t.Fatalf("outcome: got %+v want Failure with 'no choices'", outcome)
	}
}

func TestNewRejectsMissingFields(t *testing.T) {
	cases := []openaicompat.Config{
		{OpenAICompat: config.OpenAICompatConfig{APIKey: "k", Model: "m"}, Workspace: makeWorkspace(t), Prompt: makePrompt(t, "x")},
		{OpenAICompat: config.OpenAICompatConfig{Endpoint: "https://x", Model: "m"}, Workspace: makeWorkspace(t), Prompt: makePrompt(t, "x")},
		{OpenAICompat: config.OpenAICompatConfig{Endpoint: "https://x", APIKey: "k"}, Workspace: makeWorkspace(t), Prompt: makePrompt(t, "x")},
		{OpenAICompat: config.OpenAICompatConfig{Endpoint: "https://x", APIKey: "k", Model: "m"}, Prompt: makePrompt(t, "x")},
		{OpenAICompat: config.OpenAICompatConfig{Endpoint: "https://x", APIKey: "k", Model: "m"}, Workspace: makeWorkspace(t)},
	}
	for i, c := range cases {
		if _, err := openaicompat.New(c); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

type chatRequestCapture struct {
	Auth string
	Body map[string]any
}
