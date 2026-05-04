package claudecode_test

import (
	"context"
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/backend/claudecode"
	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/prompt"
	"github.com/noeljackson/symphony/go/internal/workspace"
)

// fakeClaudeSuccess emits system/init, reads one user message line from
// stdin, then emits result/success with usage.
const fakeClaudeSuccess = `
echo '{"type":"system","subtype":"init","cwd":"'"$PWD"'","session_id":"sess-fake-1","tools":[],"mcp_servers":[],"model":"fake","permissionMode":"bypassPermissions","apiKeySource":"env"}'
read -r line
echo '{"type":"assistant","content":[{"type":"text","text":"plan: do the thing"}]}'
echo '{"type":"result","subtype":"success","session_id":"sess-fake-1","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}'
`

const fakeClaudeError = `
echo '{"type":"system","subtype":"init","model":"fake","session_id":"x"}'
read -r line
echo '{"type":"result","subtype":"error","message":"context length exceeded"}'
`

const fakeClaudeNoInit = `
echo 'this is not JSON'
`

const fakeClaudeMissingResult = `
echo '{"type":"system","subtype":"init","model":"fake","session_id":"x"}'
read -r line
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

func newRunner(t *testing.T, command string) *claudecode.Runner {
	t.Helper()
	r, err := claudecode.New(claudecode.Config{
		ClaudeCode: config.ClaudeCodeConfig{Command: command, TurnTimeoutMS: 10_000},
		Agent:      config.AgentConfig{MaxTurns: 1},
		Workspace:  makeWorkspace(t),
		Prompt:     makePrompt(t, "Issue {{ issue.identifier }}"),
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
	r := newRunner(t, fakeClaudeSuccess)
	events := make(chan orchestrator.AgentEvent, 32)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeSuccess {
		t.Fatalf("outcome: got %v want Success (err=%q)", outcome.Kind, outcome.Error)
	}
	got := drainEvents(events, 100*time.Millisecond)
	if len(got) < 2 || got[0].Event != "session_started" {
		t.Fatalf("first event: got %+v want session_started first", got)
	}
	last := got[len(got)-1]
	if last.Event != "turn_completed" {
		t.Fatalf("last event: got %q want turn_completed", last.Event)
	}
	wantSubstr := "in=10 out=5 total=15"
	if !contains(last.Message, wantSubstr) {
		t.Fatalf("turn_completed: got %q want substring %q", last.Message, wantSubstr)
	}
	// Assistant text from the middle event.
	foundAssistant := false
	for _, ev := range got {
		if ev.Event == "assistant_message" && ev.Message == "plan: do the thing" {
			foundAssistant = true
		}
	}
	if !foundAssistant {
		t.Fatalf("assistant_message missing from events: %+v", got)
	}
}

func TestRunSurfacesResultError(t *testing.T) {
	r := newRunner(t, fakeClaudeError)
	events := make(chan orchestrator.AgentEvent, 8)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure {
		t.Fatalf("outcome: got %v want Failure", outcome.Kind)
	}
	if !contains(outcome.Error, "context length exceeded") {
		t.Fatalf("error: got %q", outcome.Error)
	}
}

func TestRunFailsWhenInitMissing(t *testing.T) {
	r := newRunner(t, fakeClaudeNoInit)
	events := make(chan orchestrator.AgentEvent, 4)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure {
		t.Fatalf("outcome: got %v want Failure", outcome.Kind)
	}
	if !contains(outcome.Error, "startup_failed") {
		t.Fatalf("error: got %q want substring startup_failed", outcome.Error)
	}
}

func TestRunFailsWhenStreamClosesBeforeResult(t *testing.T) {
	r := newRunner(t, fakeClaudeMissingResult)
	events := make(chan orchestrator.AgentEvent, 8)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure {
		t.Fatalf("outcome: got %v want Failure", outcome.Kind)
	}
	if !contains(outcome.Error, "stream closed before result") {
		t.Fatalf("error: got %q want substring 'stream closed before result'", outcome.Error)
	}
}

func TestRunFailsWhenCommandMissing(t *testing.T) {
	r := newRunner(t, "definitely-not-on-PATH-claude-test --print")
	events := make(chan orchestrator.AgentEvent, 4)
	outcome := r.Run(context.Background(), mkIssue("MT-1"), nil, events)
	close(events)
	if outcome.Kind != orchestrator.WorkerOutcomeFailure {
		t.Fatalf("outcome: got %v want Failure", outcome.Kind)
	}
	if !contains(outcome.Error, "startup_failed") &&
		!contains(outcome.Error, "agent_runner_not_found") {
		t.Fatalf("error: got %q want startup_failed or agent_runner_not_found", outcome.Error)
	}
}

func TestNewRejectsMissingFields(t *testing.T) {
	cases := []claudecode.Config{
		// missing command
		{ClaudeCode: config.ClaudeCodeConfig{}, Agent: config.AgentConfig{}, Workspace: makeWorkspace(t), Prompt: makePrompt(t, "x")},
		// missing workspace
		{ClaudeCode: config.ClaudeCodeConfig{Command: "true"}, Agent: config.AgentConfig{}, Prompt: makePrompt(t, "x")},
		// missing prompt
		{ClaudeCode: config.ClaudeCodeConfig{Command: "true"}, Agent: config.AgentConfig{}, Workspace: makeWorkspace(t)},
	}
	for i, c := range cases {
		if _, err := claudecode.New(c); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
