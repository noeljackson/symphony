// Package issue defines the canonical Issue and Blocker types per SPEC §4.1.1.
package issue

import (
	"strings"
	"time"
)

// Issue is the orchestrator's view of one tracked work item.
//
// All fields except ID, Identifier, Title, and State are optional; the
// dispatch eligibility predicate (§16.2) treats missing required fields
// as a hard fail rather than coercing them.
type Issue struct {
	ID          string     `json:"id"`
	Identifier  string     `json:"identifier"`
	Title       string     `json:"title"`
	Description *string    `json:"description,omitempty"`
	Priority    *int       `json:"priority,omitempty"`
	State       string     `json:"state"`
	BranchName  *string    `json:"branch_name,omitempty"`
	URL         *string    `json:"url,omitempty"`
	Labels      []string   `json:"labels,omitempty"`
	BlockedBy   []Blocker  `json:"blocked_by,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
}

// Blocker references another issue that gates dispatch of this one.
//
// SPEC §16.2: a Todo issue with any non-terminal blocker is held back.
type Blocker struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	State      string `json:"state"`
}

// NormalizedState returns the lowercased state per SPEC §4.2.
func (i *Issue) NormalizedState() string {
	return strings.ToLower(i.State)
}

// BlockersAllTerminal reports whether every entry in BlockedBy is in one
// of the configured terminal states (case-insensitive).
//
// An issue with no blockers trivially satisfies this predicate.
func (i *Issue) BlockersAllTerminal(terminalStates []string) bool {
	if len(i.BlockedBy) == 0 {
		return true
	}
	for _, b := range i.BlockedBy {
		matched := false
		for _, ts := range terminalStates {
			if strings.EqualFold(b.State, ts) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
