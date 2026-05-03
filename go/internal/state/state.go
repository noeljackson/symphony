// Package state holds the orchestrator's single-authority state and the pure
// helpers that mutate cost and event counters per SPEC §4.1.8 / §13.5 /
// §13.7.2.
//
// The OrchestratorState is owned by exactly one goroutine (the actor in
// internal/orchestrator). All mutations go through that goroutine; pure
// helpers in this package operate on a *OrchestratorState the caller holds
// exclusive access to.
package state

import (
	"time"

	"github.com/noeljackson/symphony/go/internal/issue"
)

// RecentEventsCap is the SPEC §13.7.2 RECOMMENDED ring-buffer depth.
const RecentEventsCap = 50

// RecentEvent is one entry in a per-issue ring buffer (§13.7.2).
type RecentEvent struct {
	At      time.Time
	Event   string
	Message string
}

// LiveSession captures SPEC §4.1.6 per-issue agent-session metadata.
type LiveSession struct {
	SessionID                  string
	ThreadID                   string
	TurnID                     string
	AgentRunnerPID             string
	LastAgentEvent             string
	LastAgentMessage           string
	LastAgentTimestamp         *time.Time
	LastAgentTimestampMonotone *time.Time
	AgentInputTokens           uint64
	AgentOutputTokens          uint64
	AgentTotalTokens           uint64
	LastReportedInputTokens    uint64
	LastReportedOutputTokens   uint64
	LastReportedTotalTokens    uint64
	TurnCount                  uint32
	Model                      string
	RecentEvents               []RecentEvent
}

// RunningEntry is one row in OrchestratorState.Running (§16.4).
type RunningEntry struct {
	Identifier      string
	Issue           issue.Issue
	Session         LiveSession
	RetryAttempt    *uint32
	StartedAt       time.Time
	StartedMonotone time.Time
}

// RetryEntry is one queued retry waiting for its backoff to elapse (§4.1.7).
type RetryEntry struct {
	IssueID    string
	Identifier string
	Attempt    uint32
	DueAt      time.Time
	Error      string
}

// AgentTotals tracks aggregate tokens, runtime seconds, and USD cost (§13.3 / §13.5).
//
// CostUSD and CostUSDToday are nil when the implementation cannot price the
// configured backend. Per SPEC §13.5 a nil cost MUST disable budget-cap
// enforcement.
type AgentTotals struct {
	InputTokens    uint64
	OutputTokens   uint64
	TotalTokens    uint64
	SecondsRunning float64
	CostUSD        *float64
	CostUSDToday   *float64
}

// OrchestratorState is the single-authority runtime view (§4.1.8).
//
// Owned by exactly one goroutine (the orchestrator actor). Fields tagged
// "persisted" in SPEC §4.1.9 are reloaded on restart; everything else is
// rebuilt or empty after a process restart.
type OrchestratorState struct {
	PollIntervalMS       uint64
	MaxConcurrentAgents  int
	Running              map[string]*RunningEntry
	Claimed              map[string]struct{}    // persisted
	RetryAttempts        map[string]*RetryEntry // persisted
	Completed            map[string]struct{}
	AgentTotals          AgentTotals // persisted (cost fields)
	AgentRateLimits      any
	DailyCostWindow      *time.Time // persisted (UTC date marker)
	LastBudgetWarningPct *uint32    // persisted
}

// NewState returns a zeroed OrchestratorState ready for the actor to fill in.
func NewState() *OrchestratorState {
	return &OrchestratorState{
		Running:       map[string]*RunningEntry{},
		Claimed:       map[string]struct{}{},
		RetryAttempts: map[string]*RetryEntry{},
		Completed:     map[string]struct{}{},
	}
}
