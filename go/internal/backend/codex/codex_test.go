package codex_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/backend/codex"
	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/prompt"
	"github.com/noeljackson/symphony/go/internal/workspace"
)

// fakeCodexSuccess emulates the codex app-server protocol enough to drive
// one turn end-to-end:
//   - reads the `initialize` request, replies with a result.
//   - reads the `initialized` notification (drops it).
//   - reads the `thread/start` request, replies with a thread id.
//   - reads the `turn/start` request, replies with a turn id.
//   - emits an item/assistantMessage notification.
//   - emits a thread/tokenUsage/updated notification.
//   - emits a turn/completed notification.
//
// Each request from the client carries an `id`; the fake echoes that id
// in its response. We use a tiny Python-free awk script so the test
// environment doesn't need anything beyond bash.
const fakeCodexSuccess = `
read -r init
init_id=$(echo "$init" | awk -F'"id":' '{print $2}' | awk -F',' '{print $1}')
echo "{\"jsonrpc\":\"2.0\",\"id\":${init_id},\"result\":{}}"
read -r notif # initialized
read -r start
start_id=$(echo "$start" | awk -F'"id":' '{print $2}' | awk -F',' '{print $1}')
echo "{\"jsonrpc\":\"2.0\",\"id\":${start_id},\"result\":{\"thread\":{\"id\":\"thr-1\"}}}"
read -r turn
turn_id=$(echo "$turn" | awk -F'"id":' '{print $2}' | awk -F',' '{print $1}')
echo "{\"jsonrpc\":\"2.0\",\"id\":${turn_id},\"result\":{\"turn\":{\"id\":\"trn-1\"}}}"
echo '{"jsonrpc":"2.0","method":"item/assistantMessage","params":{"content":[{"type":"text","text":"plan"}]}}'
echo '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}'
echo '{"jsonrpc":"2.0","method":"turn/completed","params":{}}'
`

const fakeCodexTurnFailed = `
read -r init
init_id=$(echo "$init" | awk -F'"id":' '{print $2}' | awk -F',' '{print $1}')
echo "{\"jsonrpc\":\"2.0\",\"id\":${init_id},\"result\":{}}"
read -r notif
read -r start
start_id=$(echo "$start" | awk -F'"id":' '{print $2}' | awk -F',' '{print $1}')
echo "{\"jsonrpc\":\"2.0\",\"id\":${start_id},\"result\":{\"thread\":{\"id\":\"thr-1\"}}}"
read -r turn
turn_id=$(echo "$turn" | awk -F'"id":' '{print $2}' | awk -F',' '{print $1}')
echo "{\"jsonrpc\":\"2.0\",\"id\":${turn_id},\"result\":{\"turn\":{\"id\":\"trn-1\"}}}"
echo '{"jsonrpc":"2.0","method":"turn/failed","params":{"message":"context length exceeded"}}'
`

const fakeCodexThreadStartError = `
read -r init
init_id=$(echo "$init" | awk -F'"id":' '{print $2}' | awk -F',' '{print $1}')
echo "{\"jsonrpc\":\"2.0\",\"id\":${init_id},\"result\":{}}"
read -r notif
read -r start
start_id=$(echo "$start" | awk -F'"id":' '{print $2}' | awk -F',' '{print $1}')
echo "{\"jsonrpc\":\"2.0\",\"id\":${start_id},\"error\":{\"code\":-32000,\"message\":\"sandbox not available\"}}"
`

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

func newRunner(t *testing.T, command string) *codex.Runner {
	t.Helper()
	r, err := codex.New(codex.Config{
		Codex:     config.CodexConfig{Command: command, TurnTimeoutMS: 10_000},
		Agent:     config.AgentConfig{MaxTurns: 1},
		Workspace: makeWorkspace(t),
		Prompt:    makePrompt(t, "Issue {{ issue.identifier }}"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
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

func TestRunSuccessEmitsLifecycleAndUsage(t *testing.T) {
	r := newRunner(t, fakeCodexSuccess)
	events := make(chan orchestrator.AgentEvent, 32)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeSuccess {
		t.Fatalf("outcome: got %v want Success (err=%q)", outcome.Kind, outcome.Error)
	}
	got := drainEvents(events, 200*time.Millisecond)
	if len(got) < 2 || got[0].Event != "session_started" {
		t.Fatalf("first event: got %+v want session_started first", got)
	}
	last := got[len(got)-1]
	if last.Event != "turn_completed" {
		t.Fatalf("last event: got %q want turn_completed", last.Event)
	}
	if !strings.Contains(last.Message, "in=100 out=50 total=150") {
		t.Fatalf("turn_completed missing usage: %q", last.Message)
	}
	foundAssistant := false
	for _, ev := range got {
		if ev.Event == "assistant_message" && ev.Message == "plan" {
			foundAssistant = true
		}
	}
	if !foundAssistant {
		t.Fatalf("assistant_message missing: %+v", got)
	}
}

func TestRunSurfacesTurnFailure(t *testing.T) {
	r := newRunner(t, fakeCodexTurnFailed)
	events := make(chan orchestrator.AgentEvent, 16)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure {
		t.Fatalf("outcome: got %v want Failure", outcome.Kind)
	}
	if !strings.Contains(outcome.Error, "context length exceeded") {
		t.Fatalf("error: got %q", outcome.Error)
	}
}

func TestRunSurfacesThreadStartError(t *testing.T) {
	r := newRunner(t, fakeCodexThreadStartError)
	events := make(chan orchestrator.AgentEvent, 4)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure {
		t.Fatalf("outcome: got %v want Failure", outcome.Kind)
	}
	if !strings.Contains(outcome.Error, "sandbox not available") {
		t.Fatalf("error: got %q", outcome.Error)
	}
}

func TestRunFailsWhenCommandMissing(t *testing.T) {
	r := newRunner(t, "definitely-not-on-PATH-codex --app-server")
	events := make(chan orchestrator.AgentEvent, 4)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure {
		t.Fatalf("outcome: got %v want Failure", outcome.Kind)
	}
}

func TestNewRejectsMissingFields(t *testing.T) {
	cases := []codex.Config{
		{Codex: config.CodexConfig{}, Workspace: makeWorkspace(t), Prompt: makePrompt(t, "x")},
		{Codex: config.CodexConfig{Command: "true"}, Prompt: makePrompt(t, "x")},
		{Codex: config.CodexConfig{Command: "true"}, Workspace: makeWorkspace(t)},
	}
	for i, c := range cases {
		if _, err := codex.New(c); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}
