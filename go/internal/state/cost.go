package state

import "time"

// RollOverDailyCost implements the SPEC §13.5 lazy daily-window rollover.
//
// When the active window date is stale (or unset), this resets
// AgentTotals.CostUSDToday to *0.0 if any cost has ever been recorded
// (otherwise nil so the cap stays inert per §13.5), and clears the
// LastBudgetWarningPct suppressor.
func RollOverDailyCost(s *OrchestratorState, today time.Time) {
	today = utcDate(today)
	if s.DailyCostWindow != nil && s.DailyCostWindow.Equal(today) {
		return
	}
	s.DailyCostWindow = &today
	if s.AgentTotals.CostUSD != nil {
		zero := 0.0
		s.AgentTotals.CostUSDToday = &zero
	} else {
		s.AgentTotals.CostUSDToday = nil
	}
	s.LastBudgetWarningPct = nil
}

// AddCost rolls over the daily window first, then accumulates `delta` against
// both the lifetime and per-day counters. Always promotes nil totals to
// *(0.0 + delta) because the caller has just produced a real cost (§13.5).
func AddCost(s *OrchestratorState, delta float64, today time.Time) {
	RollOverDailyCost(s, today)
	lifetime := delta
	if s.AgentTotals.CostUSD != nil {
		lifetime += *s.AgentTotals.CostUSD
	}
	dayTotal := delta
	if s.AgentTotals.CostUSDToday != nil {
		dayTotal += *s.AgentTotals.CostUSDToday
	}
	s.AgentTotals.CostUSD = &lifetime
	s.AgentTotals.CostUSDToday = &dayTotal
}

// BudgetCapReached reports whether dispatch must be skipped because the
// cumulative daily cost has reached the cap (SPEC §13.5 / §16.2).
//
// Returns false (cap inert) when either the cap or CostUSDToday is unset:
// SPEC §13.5 mandates that nil cost MUST NOT be silently treated as zero.
func BudgetCapReached(s *OrchestratorState, dailyBudgetUSD *float64) bool {
	if dailyBudgetUSD == nil || s.AgentTotals.CostUSDToday == nil {
		return false
	}
	return *s.AgentTotals.CostUSDToday >= *dailyBudgetUSD
}

func utcDate(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
