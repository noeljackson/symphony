package store

import (
	"context"
	"sync"
	"time"

	"github.com/noeljackson/symphony/go/internal/state"
)

// MemoryStore is an in-memory implementation of [Store] used by tests and
// by the orchestrator's smoke-test harness. Production uses Postgres
// (forthcoming in a separate PR).
type MemoryStore struct {
	mu                   sync.Mutex
	totals               state.AgentTotals
	dailyCostWindow      *time.Time
	lastBudgetWarningPct *uint32
	retries              map[string]*state.RetryEntry
	claimed              map[string]struct{}
	recentEvents         map[string][]state.RecentEvent
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		retries:      map[string]*state.RetryEntry{},
		claimed:      map[string]struct{}{},
		recentEvents: map[string][]state.RecentEvent{},
	}
}

func (s *MemoryStore) Restore(_ context.Context) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	retries := make(map[string]*state.RetryEntry, len(s.retries))
	for k, v := range s.retries {
		copy := *v
		retries[k] = &copy
	}
	claimed := make(map[string]struct{}, len(s.claimed))
	for k := range s.claimed {
		claimed[k] = struct{}{}
	}
	events := make(map[string][]state.RecentEvent, len(s.recentEvents))
	for k, v := range s.recentEvents {
		events[k] = append([]state.RecentEvent(nil), v...)
	}
	return &Snapshot{
		AgentTotals:          s.totals,
		DailyCostWindow:      s.dailyCostWindow,
		LastBudgetWarningPct: s.lastBudgetWarningPct,
		RetryAttempts:        retries,
		Claimed:              claimed,
		RecentEventsByIssue:  events,
	}, nil
}

func (s *MemoryStore) SaveAgentTotals(_ context.Context, totals state.AgentTotals, window *time.Time, warningPct *uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totals = totals
	s.dailyCostWindow = window
	s.lastBudgetWarningPct = warningPct
	return nil
}

func (s *MemoryStore) UpsertRetry(_ context.Context, entry state.RetryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := entry
	s.retries[entry.IssueID] = &copy
	return nil
}

func (s *MemoryStore) DeleteRetry(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.retries, issueID)
	return nil
}

func (s *MemoryStore) SetClaimed(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimed[issueID] = struct{}{}
	return nil
}

func (s *MemoryStore) ClearClaimed(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claimed, issueID)
	return nil
}

func (s *MemoryStore) AppendRecentEvent(_ context.Context, issueID string, ev state.RecentEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.recentEvents[issueID]
	buf = state.PushRecentEvent(buf, ev)
	s.recentEvents[issueID] = buf
	return nil
}

func (s *MemoryStore) ClearRecentEventsForIssue(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recentEvents, issueID)
	return nil
}
