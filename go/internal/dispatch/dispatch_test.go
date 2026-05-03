package dispatch

import (
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/state"
)

func cfg(maxConcurrent int, byState map[string]int) *config.ServiceConfig {
	return &config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           config.TrackerLinear,
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Done", "Cancelled"},
		},
		Agent: config.AgentConfig{
			Backend:                    config.BackendCodex,
			MaxConcurrentAgents:        maxConcurrent,
			MaxRetryBackoffMS:          300_000,
			MaxConcurrentAgentsByState: byState,
		},
	}
}

func mkIssue(id, ident string, priority *int, st string, created *time.Time) issue.Issue {
	return issue.Issue{
		ID:         id,
		Identifier: ident,
		Title:      "t",
		Priority:   priority,
		State:      st,
		CreatedAt:  created,
	}
}

func TestCheckRejectsMissingFields(t *testing.T) {
	c := cfg(10, nil)
	s := state.NewState()
	i := issue.Issue{ID: "", Identifier: "MT-1", Title: "t", State: "Todo"}
	v := Check(&i, c, s)
	if v.Reason != VerdictMissingFields {
		t.Fatalf("got %v want VerdictMissingFields", v.Reason)
	}
}

func TestCheckRejectsTerminalState(t *testing.T) {
	c := cfg(10, nil)
	s := state.NewState()
	i := mkIssue("a", "MT-1", nil, "Done", nil)
	v := Check(&i, c, s)
	if v.Reason != VerdictInTerminalStates {
		t.Fatalf("got %v want VerdictInTerminalStates", v.Reason)
	}
}

func TestCheckRejectsNonActiveState(t *testing.T) {
	c := cfg(10, nil)
	s := state.NewState()
	i := mkIssue("a", "MT-1", nil, "Triage", nil)
	v := Check(&i, c, s)
	if v.Reason != VerdictNotInActiveStates {
		t.Fatalf("got %v want VerdictNotInActiveStates", v.Reason)
	}
}

func TestCheckHonorsGlobalConcurrencyCap(t *testing.T) {
	c := cfg(1, nil)
	s := state.NewState()
	s.Running["x"] = &state.RunningEntry{Identifier: "MT-X", Issue: mkIssue("x", "MT-X", nil, "Todo", nil)}
	i := mkIssue("a", "MT-1", nil, "Todo", nil)
	v := Check(&i, c, s)
	if v.Reason != VerdictGlobalSlotsExhausted {
		t.Fatalf("got %v want VerdictGlobalSlotsExhausted", v.Reason)
	}
}

func TestCheckHonorsPerStateCap(t *testing.T) {
	c := cfg(10, map[string]int{"todo": 1})
	s := state.NewState()
	s.Running["x"] = &state.RunningEntry{Identifier: "MT-X", Issue: mkIssue("x", "MT-X", nil, "Todo", nil)}
	i := mkIssue("a", "MT-1", nil, "Todo", nil)
	v := Check(&i, c, s)
	if v.Reason != VerdictPerStateSlotsExhausted {
		t.Fatalf("got %v want VerdictPerStateSlotsExhausted", v.Reason)
	}
}

func TestCheckRejectsBlockedTodo(t *testing.T) {
	c := cfg(10, nil)
	s := state.NewState()
	i := mkIssue("a", "MT-1", nil, "Todo", nil)
	i.BlockedBy = []issue.Blocker{{ID: "x", Identifier: "MT-X", State: "Todo"}}
	v := Check(&i, c, s)
	if v.Reason != VerdictBlockedByOpenBlocker {
		t.Fatalf("got %v want VerdictBlockedByOpenBlocker", v.Reason)
	}
}

func TestCheckEligibleForActiveTodo(t *testing.T) {
	c := cfg(10, nil)
	s := state.NewState()
	i := mkIssue("a", "MT-1", nil, "Todo", nil)
	v := Check(&i, c, s)
	if !v.Eligible {
		t.Fatalf("expected eligible, got %v", v.Reason)
	}
}

func TestSortForDispatchPriorityThenCreatedThenIdentifier(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	prio1 := 1
	prio3 := 3
	issues := []issue.Issue{
		mkIssue("c", "MT-3", nil, "Todo", &t2),    // unprioritized -> last bucket
		mkIssue("b", "MT-1", &prio3, "Todo", &t1), // prio 3, t1
		mkIssue("a", "MT-2", &prio1, "Todo", &t2), // prio 1, t2
	}
	SortForDispatch(issues)
	got := []string{issues[0].Identifier, issues[1].Identifier, issues[2].Identifier}
	want := []string{"MT-2", "MT-1", "MT-3"}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("sort order: got %v want %v", got, want)
		}
	}
}

func TestRetryDelayContinuationIs1s(t *testing.T) {
	if got := RetryDelay(1, 300_000, true); got != time.Second {
		t.Fatalf("continuation: got %v want 1s", got)
	}
}

func TestRetryDelayExponentialAndCapped(t *testing.T) {
	// attempt=1 -> 10s, attempt=2 -> 20s, attempt=3 -> 40s
	if got := RetryDelay(1, 300_000, false); got != 10*time.Second {
		t.Fatalf("attempt=1: got %v want 10s", got)
	}
	if got := RetryDelay(2, 300_000, false); got != 20*time.Second {
		t.Fatalf("attempt=2: got %v want 20s", got)
	}
	if got := RetryDelay(3, 300_000, false); got != 40*time.Second {
		t.Fatalf("attempt=3: got %v want 40s", got)
	}
	// Capped at max.
	if got := RetryDelay(50, 30_000, false); got != 30*time.Second {
		t.Fatalf("cap: got %v want 30s", got)
	}
}
