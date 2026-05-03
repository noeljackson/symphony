package orchestrator

import (
	"context"

	"github.com/noeljackson/symphony/go/internal/issue"
)

// WorkerOutcome is the result returned by a [WorkerRunner.Run] call when the
// worker exits (SPEC §16.4 / §8.4).
type WorkerOutcome struct {
	Kind  WorkerOutcomeKind
	Error string // populated when Kind == WorkerOutcomeFailure
}

// WorkerOutcomeKind enumerates the outcome categories.
type WorkerOutcomeKind int

const (
	WorkerOutcomeSuccess WorkerOutcomeKind = iota
	WorkerOutcomeFailure
)

// AgentEvent is a humanized snapshot of one runtime event the worker emitted
// (SPEC §10.4 / §13.5 / §13.7.4). The orchestrator forwards it to the
// recent_events ring buffer and SSE broadcast.
//
// In the foundation PR this is a small subset of the eventual shape; the
// agent-backend impl PRs will extend it with thread-total token usage,
// session/thread/turn IDs, and per-backend payload passthrough.
type AgentEvent struct {
	Event   string
	Message string
}

// WorkerRunner is the contract a backend implementation satisfies. The
// orchestrator dispatches one Run call per agent attempt and forwards each
// AgentEvent on the supplied channel.
type WorkerRunner interface {
	Run(ctx context.Context, i issue.Issue, attempt *uint32, events chan<- AgentEvent) WorkerOutcome
}
