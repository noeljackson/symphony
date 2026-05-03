// Package dispatch contains the pure-function predicates the orchestrator
// uses to decide which issues are eligible to dispatch and in what order
// (SPEC §8.2 / §8.4 / §16.2).
//
// Nothing in this package mutates orchestrator state or talks to a tracker
// — it's the unit-testable core of the dispatch loop.
package dispatch

import (
	"sort"
	"strings"
	"time"

	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/state"
)

// Verdict explains why an issue is or isn't eligible.
type Verdict int

const (
	VerdictOK Verdict = iota
	VerdictAlreadyRunning
	VerdictAlreadyClaimed
	VerdictNotInActiveStates
	VerdictInTerminalStates
	VerdictBlockedByOpenBlocker
	VerdictGlobalSlotsExhausted
	VerdictPerStateSlotsExhausted
	VerdictMissingFields
)

// Eligibility is the result of [Check]. Eligible == true iff Reason == VerdictOK.
type Eligibility struct {
	Eligible bool
	Reason   Verdict
}

// Check applies SPEC §16.2 dispatch eligibility against the supplied state +
// config. It does not mutate either.
func Check(i *issue.Issue, cfg *config.ServiceConfig, s *state.OrchestratorState) Eligibility {
	if i.ID == "" || i.Identifier == "" || i.Title == "" || i.State == "" {
		return Eligibility{Eligible: false, Reason: VerdictMissingFields}
	}
	if anyEqualFold(cfg.Tracker.TerminalStates, i.State) {
		return Eligibility{Reason: VerdictInTerminalStates}
	}
	if !anyEqualFold(cfg.Tracker.ActiveStates, i.State) {
		return Eligibility{Reason: VerdictNotInActiveStates}
	}
	if _, running := s.Running[i.ID]; running {
		return Eligibility{Reason: VerdictAlreadyRunning}
	}
	if _, claimed := s.Claimed[i.ID]; claimed {
		return Eligibility{Reason: VerdictAlreadyClaimed}
	}
	if i.NormalizedState() == "todo" && !i.BlockersAllTerminal(cfg.Tracker.TerminalStates) {
		return Eligibility{Reason: VerdictBlockedByOpenBlocker}
	}
	if GlobalAvailableSlots(cfg, s) == 0 {
		return Eligibility{Reason: VerdictGlobalSlotsExhausted}
	}
	if PerStateAvailableSlots(i.State, cfg, s) == 0 {
		return Eligibility{Reason: VerdictPerStateSlotsExhausted}
	}
	return Eligibility{Eligible: true, Reason: VerdictOK}
}

// GlobalAvailableSlots is the unfilled portion of agent.max_concurrent_agents.
func GlobalAvailableSlots(cfg *config.ServiceConfig, s *state.OrchestratorState) int {
	cap := cfg.Agent.MaxConcurrentAgents
	used := len(s.Running)
	if used >= cap {
		return 0
	}
	return cap - used
}

// PerStateAvailableSlots is the unfilled portion of the per-state cap from
// agent.max_concurrent_agents_by_state. When no per-state cap is configured,
// this falls back to the global cap minus the count of issues currently
// running in `targetState`.
func PerStateAvailableSlots(targetState string, cfg *config.ServiceConfig, s *state.OrchestratorState) int {
	key := strings.ToLower(targetState)
	cap := cfg.Agent.MaxConcurrentAgents
	if v, ok := cfg.Agent.MaxConcurrentAgentsByState[key]; ok {
		cap = v
	}
	count := countRunningInState(targetState, s)
	if count >= cap {
		return 0
	}
	return cap - count
}

func countRunningInState(targetState string, s *state.OrchestratorState) int {
	n := 0
	for _, r := range s.Running {
		if strings.EqualFold(r.Issue.State, targetState) {
			n++
		}
	}
	return n
}

// SortForDispatch reorders `issues` by SPEC §8.2: priority ascending (null
// last), created_at oldest first, identifier lexicographic tiebreak.
func SortForDispatch(issues []issue.Issue) {
	sort.Slice(issues, func(i, j int) bool {
		ai, bi := dispatchKey(&issues[i]), dispatchKey(&issues[j])
		if ai.bucket != bi.bucket {
			return ai.bucket < bi.bucket
		}
		if ai.priority != bi.priority {
			return ai.priority < bi.priority
		}
		if !ai.created.Equal(bi.created) {
			return ai.created.Before(bi.created)
		}
		return ai.identifier < bi.identifier
	})
}

type dispatchKeyT struct {
	bucket     uint8
	priority   int
	created    time.Time
	identifier string
}

func dispatchKey(i *issue.Issue) dispatchKeyT {
	bucket, priority := uint8(1), 0
	if i.Priority != nil {
		bucket, priority = 0, *i.Priority
	}
	created := time.Unix(0, 0).UTC()
	if i.CreatedAt != nil {
		created = *i.CreatedAt
	}
	return dispatchKeyT{bucket: bucket, priority: priority, created: created, identifier: i.Identifier}
}

// RetryDelay implements SPEC §8.4 backoff. Continuation retries (after a
// clean worker exit) use a short fixed delay; failure-driven retries use
// exponential backoff capped at maxCapMS.
func RetryDelay(attempt uint32, maxCapMS uint64, continuation bool) time.Duration {
	if continuation {
		return time.Second
	}
	expSteps := attempt
	if expSteps > 0 {
		expSteps -= 1
	}
	if expSteps > 20 {
		expSteps = 20
	}
	raw := uint64(10_000) * (uint64(1) << expSteps)
	if raw > maxCapMS {
		raw = maxCapMS
	}
	return time.Duration(raw) * time.Millisecond
}

// RunningCountByState groups running issues by their tracker state for
// snapshot output.
func RunningCountByState(s *state.OrchestratorState) map[string]int {
	out := map[string]int{}
	for _, r := range s.Running {
		out[r.Issue.State]++
	}
	return out
}

func anyEqualFold(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.EqualFold(s, needle) {
			return true
		}
	}
	return false
}
