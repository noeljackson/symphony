// Package tracker defines the issue-tracker abstraction (SPEC §5.3.1).
//
// Each tracker kind (linear, github) is a separate implementation behind
// the [Tracker] interface; the orchestrator stays decoupled from any one
// vendor.
package tracker

import (
	"context"

	"github.com/noeljackson/symphony/go/internal/issue"
)

// Tracker is the read-only-from-orchestrator-perspective interface every
// tracker implementation satisfies. Mutations (issue state transitions,
// comments) are performed by the agent backend via tools, not by the
// orchestrator.
type Tracker interface {
	// FetchCandidateIssues returns issues whose tracker state intersects the
	// configured ActiveStates. Implementations apply the v3 §5.3.1 active/
	// terminal-state rules at the source where possible (for example, Linear
	// passes a state filter to its GraphQL query).
	FetchCandidateIssues(ctx context.Context) ([]issue.Issue, error)

	// FetchIssueStatesByIDs is used by reconciliation to refresh in-flight
	// issues' state. Returns the matching issues with their current state.
	FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]issue.Issue, error)

	// FetchIssuesByStates returns issues whose state matches any value in
	// `states`. Used at startup for terminal-workspace cleanup (§8.6).
	FetchIssuesByStates(ctx context.Context, states []string) ([]issue.Issue, error)
}
