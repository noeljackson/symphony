// SPEC §17.8 Real Integration Profile — opt-in smoke tests against the
// live Linear GraphQL API. Skipped by default; enable with
//
//	SYMPHONY_RUN_LIVE_E2E=1 LINEAR_API_KEY=... LINEAR_PROJECT_SLUG=... \
//	    go test ./internal/tracker/linear -run Live
//
// SPEC §17.8 says skipped tests "SHOULD be reported as skipped, not silently
// treated as passed" — so we t.Skip when the env-var gate is off, but
// t.Fatal when the gate IS on but a required credential is missing. That
// way an operator who explicitly opted in sees a hard error if the
// environment isn't set up correctly, rather than silent green.
package linear_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/tracker/linear"
)

const liveGateEnv = "SYMPHONY_RUN_LIVE_E2E"

func liveGate(t *testing.T) {
	t.Helper()
	if os.Getenv(liveGateEnv) != "1" {
		t.Skipf("set %s=1 to run live Linear smoke", liveGateEnv)
	}
}

func liveCfg(t *testing.T) linear.Config {
	t.Helper()
	apiKey := os.Getenv("LINEAR_API_KEY")
	if apiKey == "" {
		t.Fatalf("%s=1 but LINEAR_API_KEY is not set", liveGateEnv)
	}
	slug := os.Getenv("LINEAR_PROJECT_SLUG")
	if slug == "" {
		t.Fatalf("%s=1 but LINEAR_PROJECT_SLUG is not set", liveGateEnv)
	}
	active := []string{"Todo", "In Progress"}
	if raw := os.Getenv("SYMPHONY_LIVE_ACTIVE_STATES"); raw != "" {
		active = splitCSV(raw)
	}
	return linear.Config{
		APIKey:         apiKey,
		ProjectSlug:    slug,
		ActiveStates:   active,
		TerminalStates: []string{"Done", "Cancelled"},
	}
}

// TestLiveLinearCandidateFetch confirms the configured candidate-issue
// query parses, the auth works, and the response normalizes against the
// real Linear API. An empty result is a success — we only need the
// round-trip to land cleanly.
func TestLiveLinearCandidateFetch(t *testing.T) {
	liveGate(t)
	cfg := liveCfg(t)

	c, err := linear.New(cfg)
	if err != nil {
		t.Fatalf("linear.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	issues, err := c.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	for _, i := range issues {
		if i.ID == "" {
			t.Fatalf("issue.ID empty: %+v", i)
		}
		if i.Identifier == "" {
			t.Fatalf("issue.Identifier empty: %+v", i)
		}
	}
	t.Logf("live Linear smoke: fetched %d issue(s) from project %q", len(issues), cfg.ProjectSlug)
}

// TestLiveLinearTerminalRefresh confirms FetchIssueStatesByIDs against
// the live API. We bootstrap the IDs from a candidate fetch, so the
// test no-ops when the project has no active issues.
func TestLiveLinearTerminalRefresh(t *testing.T) {
	liveGate(t)
	cfg := liveCfg(t)

	c, err := linear.New(cfg)
	if err != nil {
		t.Fatalf("linear.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	candidates, err := c.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no active issues available; skipping terminal-refresh smoke")
	}
	ids := make([]string, 0, len(candidates))
	for _, i := range candidates {
		ids = append(ids, i.ID)
		if len(ids) >= 5 {
			break
		}
	}
	refreshed, err := c.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if len(refreshed) == 0 {
		t.Fatalf("expected at least one refreshed issue for ids=%v", ids)
	}
	t.Logf("live Linear smoke: refreshed %d/%d issue(s)", len(refreshed), len(ids))
}

func splitCSV(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
