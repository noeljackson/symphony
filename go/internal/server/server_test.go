package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/server"
	"github.com/noeljackson/symphony/go/internal/store"
	"github.com/noeljackson/symphony/go/internal/tracker"
)

type scriptedRunner struct {
	gate chan struct{}
}

func newGatedRunner() *scriptedRunner {
	return &scriptedRunner{gate: make(chan struct{})}
}

func (r *scriptedRunner) release() { close(r.gate) }

func (r *scriptedRunner) Run(ctx context.Context, i issue.Issue, _ *uint32, events chan<- orchestrator.AgentEvent) orchestrator.WorkerOutcome {
	select {
	case events <- orchestrator.AgentEvent{Event: "session_started", Message: i.Identifier}:
	case <-ctx.Done():
		return orchestrator.WorkerOutcome{Kind: orchestrator.WorkerOutcomeFailure, Error: "ctx"}
	}
	select {
	case <-r.gate:
	case <-ctx.Done():
	}
	return orchestrator.WorkerOutcome{Kind: orchestrator.WorkerOutcomeSuccess}
}

func makeConfig() *config.ServiceConfig {
	return &config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           config.TrackerLinear,
			ActiveStates:   []string{"Todo"},
			TerminalStates: []string{"Done"},
		},
		Linear:  config.LinearConfig{Endpoint: "https://x", APIKey: "k", ProjectSlug: "demo"},
		Polling: config.PollingConfig{IntervalMS: 30_000},
		Agent: config.AgentConfig{
			Backend:                    config.BackendCodex,
			MaxConcurrentAgents:        4,
			MaxRetryBackoffMS:          300_000,
			MaxConcurrentAgentsByState: map[string]int{},
		},
		Codex: config.CodexConfig{Command: "true"},
	}
}

func mkIssue(id, ident string) issue.Issue {
	return issue.Issue{ID: id, Identifier: ident, Title: "t", State: "Todo"}
}

type harness struct {
	addr   string
	handle *orchestrator.Handle
	runner *scriptedRunner
	stop   func()
}

func bootHarness(t *testing.T, issues []issue.Issue) *harness {
	t.Helper()
	cfg := makeConfig()
	tr := tracker.NewMemoryTracker(issues)
	runner := newGatedRunner()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	o, h := orchestrator.New(cfg, tr, runner, store.NewMemoryStore(), orchestrator.Options{Logger: silent})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()

	srv, err := server.New("127.0.0.1:0", h)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Start() }()

	stop := func() {
		_ = srv.Shutdown(context.Background())
		<-srvDone
		h.Shutdown(ctx)
		runner.release()
		<-done
		cancel()
	}
	return &harness{addr: srv.Addr(), handle: h, runner: runner, stop: stop}
}

func waitFor(t *testing.T, label string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for: %s", label)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func httpGet(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestStateEndpointReportsRunningAndTotals(t *testing.T) {
	h := bootHarness(t, []issue.Issue{mkIssue("a", "MT-1")})
	defer h.stop()
	h.handle.Tick(context.Background())
	waitFor(t, "running entry", func() bool {
		snap, _ := h.handle.Snapshot(context.Background())
		return len(snap.Running) == 1
	})
	status, body := httpGet(t, "http://"+h.addr+"/api/v1/state")
	if status != http.StatusOK {
		t.Fatalf("status: got %d body=%s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	counts := got["counts"].(map[string]any)
	if int(counts["running"].(float64)) != 1 {
		t.Fatalf("running count: got %v want 1", counts["running"])
	}
	totals := got["agent_totals"].(map[string]any)
	if _, ok := totals["cost_usd"]; !ok {
		t.Fatal("expected cost_usd field in agent_totals (even if null)")
	}
}

func TestIssueEndpointReturns404ForUnknown(t *testing.T) {
	h := bootHarness(t, []issue.Issue{mkIssue("a", "MT-1")})
	defer h.stop()
	status, body := httpGet(t, "http://"+h.addr+"/api/v1/MT-NOPE")
	if status != http.StatusNotFound {
		t.Fatalf("status: got %d body=%s", status, body)
	}
	if !strings.Contains(string(body), "issue_not_found") {
		t.Fatalf("body: got %s want issue_not_found", body)
	}
}

func TestIssueEndpointReturnsRunningEntry(t *testing.T) {
	h := bootHarness(t, []issue.Issue{mkIssue("a", "MT-1")})
	defer h.stop()
	h.handle.Tick(context.Background())
	waitFor(t, "running entry", func() bool {
		snap, _ := h.handle.Snapshot(context.Background())
		return len(snap.Running) == 1 && len(snap.Running[0].RecentEvents) > 0
	})
	status, body := httpGet(t, "http://"+h.addr+"/api/v1/MT-1")
	if status != http.StatusOK {
		t.Fatalf("status: got %d body=%s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["status"] != "running" {
		t.Fatalf("status field: got %v want running", got["status"])
	}
	if got["issue_identifier"] != "MT-1" {
		t.Fatalf("identifier: got %v want MT-1", got["issue_identifier"])
	}
	recent, ok := got["recent_events"].([]any)
	if !ok || len(recent) == 0 {
		t.Fatalf("recent_events: got %v want non-empty", got["recent_events"])
	}
}

func TestRefreshReturns202WithQueuedPayload(t *testing.T) {
	h := bootHarness(t, nil)
	defer h.stop()
	resp, err := http.Post("http://"+h.addr+"/api/v1/refresh", "application/json", nil)
	if err != nil {
		t.Fatalf("POST refresh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status: got %d want 202", resp.StatusCode)
	}
	var got map[string]any
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, &got)
	if got["queued"] != true {
		t.Fatalf("queued: got %v want true", got["queued"])
	}
	ops := got["operations"].([]any)
	if ops[0] != "poll" {
		t.Fatalf("operations[0]: got %v want poll", ops[0])
	}
}

func TestStateRejectsNonGet(t *testing.T) {
	h := bootHarness(t, nil)
	defer h.stop()
	resp, err := http.Post("http://"+h.addr+"/api/v1/state", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d want 405", resp.StatusCode)
	}
}

func TestSSEEmitsInitialSnapshotAndLiveEvent(t *testing.T) {
	h := bootHarness(t, []issue.Issue{mkIssue("a", "MT-1")})
	defer h.stop()

	// Open SSE stream first so we don't miss the initial snapshot or the
	// subsequent agent event the dispatch will produce.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+h.addr+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type: got %q want text/event-stream", got)
	}

	r := bufio.NewReader(resp.Body)
	gotSnapshot := waitForSSEEvent(t, r, "snapshot")
	if !strings.Contains(gotSnapshot, "agent_totals") {
		t.Fatalf("snapshot data missing agent_totals: %q", gotSnapshot)
	}

	h.handle.Tick(context.Background())
	gotLive := waitForSSEEvent(t, r, "session_started")
	if !strings.Contains(gotLive, "MT-1") {
		t.Fatalf("live event data missing identifier: %q", gotLive)
	}
}

// waitForSSEEvent reads from r until it sees an event with the given name,
// then returns the data line. Fails the test on timeout (the bufio reader
// is wired to a context-cancelled response body so the read returns
// promptly when ctx times out).
func waitForSSEEvent(t *testing.T, r *bufio.Reader, want string) string {
	t.Helper()
	var currentEvent string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v (waited for event %q)", err, want)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			currentEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			if currentEvent == want {
				return data
			}
		case line == "":
			currentEvent = ""
		}
	}
}
