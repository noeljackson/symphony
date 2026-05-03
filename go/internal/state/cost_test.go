package state

import (
	"testing"
	"time"
)

func TestAddCostRollsOverDailyWindow(t *testing.T) {
	s := NewState()
	day1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	AddCost(s, 0.50, day1)
	if got := *s.AgentTotals.CostUSD; got != 0.50 {
		t.Fatalf("lifetime cost: got %v want 0.50", got)
	}
	if got := *s.AgentTotals.CostUSDToday; got != 0.50 {
		t.Fatalf("today cost: got %v want 0.50", got)
	}

	// Same UTC day: accumulates.
	AddCost(s, 0.25, day1.Add(time.Hour))
	if got := *s.AgentTotals.CostUSD; got != 0.75 {
		t.Fatalf("lifetime cost (same day): got %v want 0.75", got)
	}
	if got := *s.AgentTotals.CostUSDToday; got != 0.75 {
		t.Fatalf("today cost (same day): got %v want 0.75", got)
	}

	// Next day: lifetime persists, daily resets to 0 + delta.
	day2 := day1.Add(24 * time.Hour)
	AddCost(s, 0.10, day2)
	if got := *s.AgentTotals.CostUSD; got != 0.85 {
		t.Fatalf("lifetime cost (next day): got %v want 0.85", got)
	}
	if got := *s.AgentTotals.CostUSDToday; got != 0.10 {
		t.Fatalf("today cost (next day): got %v want 0.10", got)
	}
}

func TestRollOverKeepsTodayNilWhenPricingUnknown(t *testing.T) {
	s := NewState()
	day1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	RollOverDailyCost(s, day1)
	if s.DailyCostWindow == nil || !s.DailyCostWindow.Equal(day1) {
		t.Fatalf("daily window: got %v want %v", s.DailyCostWindow, day1)
	}
	if s.AgentTotals.CostUSDToday != nil {
		t.Fatalf("expected today cost to stay nil with no pricing yet, got %v", *s.AgentTotals.CostUSDToday)
	}
}

func TestBudgetCapInertWithoutPricing(t *testing.T) {
	s := NewState()
	RollOverDailyCost(s, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	cap := 1.0
	if BudgetCapReached(s, &cap) {
		t.Fatal("cap must be inert when CostUSDToday is nil (SPEC §13.5)")
	}
}

func TestBudgetCapBlocksAtExactThreshold(t *testing.T) {
	s := NewState()
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	AddCost(s, 1.0, day)
	cap1 := 1.0
	cap05 := 0.5
	cap15 := 1.5
	if !BudgetCapReached(s, &cap1) {
		t.Fatal("cap == today should block")
	}
	if !BudgetCapReached(s, &cap05) {
		t.Fatal("cap < today should block")
	}
	if BudgetCapReached(s, &cap15) {
		t.Fatal("cap > today should allow")
	}
	if BudgetCapReached(s, nil) {
		t.Fatal("nil cap should allow")
	}
}

func TestRollOverClearsWarningSuppressor(t *testing.T) {
	s := NewState()
	day1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	AddCost(s, 0.50, day1)
	eighty := uint32(80)
	s.LastBudgetWarningPct = &eighty
	RollOverDailyCost(s, day1.Add(24*time.Hour))
	if s.LastBudgetWarningPct != nil {
		t.Fatalf("expected warning suppressor cleared on rollover, got %v", *s.LastBudgetWarningPct)
	}
}
