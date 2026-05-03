package tracker

import (
	"context"
	"strings"
	"sync"

	"github.com/noeljackson/symphony/go/internal/issue"
)

// MemoryTracker is an in-memory implementation used by tests. Replace returns
// allow tests to swap the issue list mid-run.
type MemoryTracker struct {
	mu     sync.Mutex
	issues []issue.Issue
}

// NewMemoryTracker returns a tracker pre-populated with `issues`.
func NewMemoryTracker(issues []issue.Issue) *MemoryTracker {
	return &MemoryTracker{issues: cloneIssues(issues)}
}

// Replace swaps the in-memory issue list (test helper).
func (m *MemoryTracker) Replace(issues []issue.Issue) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.issues = cloneIssues(issues)
}

func (m *MemoryTracker) FetchCandidateIssues(_ context.Context) ([]issue.Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneIssues(m.issues), nil
}

func (m *MemoryTracker) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]issue.Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	out := make([]issue.Issue, 0, len(ids))
	for _, i := range m.issues {
		if _, ok := wanted[i.ID]; ok {
			out = append(out, cloneIssue(i))
		}
	}
	return out, nil
}

func (m *MemoryTracker) FetchIssuesByStates(_ context.Context, states []string) ([]issue.Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]issue.Issue, 0, len(m.issues))
	for _, i := range m.issues {
		for _, s := range states {
			if strings.EqualFold(i.State, s) {
				out = append(out, cloneIssue(i))
				break
			}
		}
	}
	return out, nil
}

func cloneIssues(in []issue.Issue) []issue.Issue {
	out := make([]issue.Issue, len(in))
	for i, v := range in {
		out[i] = cloneIssue(v)
	}
	return out
}

func cloneIssue(in issue.Issue) issue.Issue {
	out := in
	if in.Labels != nil {
		out.Labels = append([]string(nil), in.Labels...)
	}
	if in.BlockedBy != nil {
		out.BlockedBy = append([]issue.Blocker(nil), in.BlockedBy...)
	}
	return out
}
