package dashboard_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/dashboard"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/store"
	"github.com/noeljackson/symphony/go/internal/tracker"
)

type scriptedRunner struct {
	gateOnce sync.Once
	gate     chan struct{}
}

func newRunner() *scriptedRunner {
	return &scriptedRunner{gate: make(chan struct{})}
}

func (r *scriptedRunner) release() {
	r.gateOnce.Do(func() { close(r.gate) })
}

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

func boot(t *testing.T, issues []issue.Issue) (string, *orchestrator.Handle, *scriptedRunner, func()) {
	t.Helper()
	cfg := &config.ServiceConfig{
		Tracker: config.TrackerConfig{Kind: config.TrackerLinear, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}},
		Linear:  config.LinearConfig{Endpoint: "https://x", APIKey: "k", ProjectSlug: "demo"},
		Polling: config.PollingConfig{IntervalMS: 30_000},
		Agent:   config.AgentConfig{Backend: config.BackendCodex, MaxConcurrentAgents: 4, MaxRetryBackoffMS: 300_000, MaxConcurrentAgentsByState: map[string]int{}},
		Codex:   config.CodexConfig{Command: "true"},
	}
	tr := tracker.NewMemoryTracker(issues)
	r := newRunner()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	o, h := orchestrator.New(cfg, tr, r, store.NewMemoryStore(), orchestrator.Options{Logger: silent})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.Run(ctx); close(done) }()

	mux := http.NewServeMux()
	dashboard.Mount(mux, h)
	srv := httptest.NewServer(mux)

	stop := func() {
		srv.Close()
		h.Shutdown(ctx)
		r.release()
		<-done
		cancel()
	}
	return srv.URL, h, r, stop
}

func mkIssue(id, ident string) issue.Issue {
	return issue.Issue{ID: id, Identifier: ident, Title: "t", State: "Todo"}
}

func waitFor(t *testing.T, label string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", label)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestRootRendersHTMLWithCoreSections(t *testing.T) {
	url, h, runner, stop := boot(t, []issue.Issue{mkIssue("a", "MT-1")})
	defer stop()
	defer runner.release()
	h.Tick(context.Background())
	waitFor(t, "running entry", func() bool {
		s, _ := h.Snapshot(context.Background())
		return len(s.Running) == 1
	})

	status, body := httpGet(t, url+"/")
	if status != http.StatusOK {
		t.Fatalf("status: got %d body=%s", status, body)
	}
	for _, want := range []string{
		`<title>Symphony — Dashboard</title>`,
		`id="running"`,
		`id="retrying"`,
		`id="totals"`,
		`MT-1`,
		`Operations Dashboard`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
}

func TestRootHEADRejected(t *testing.T) {
	url, _, _, stop := boot(t, nil)
	defer stop()
	resp, err := http.Head(url + "/")
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d want 405", resp.StatusCode)
	}
}

func TestStaticCSSServed(t *testing.T) {
	url, _, _, stop := boot(t, nil)
	defer stop()
	status, body := httpGet(t, url+"/dashboard.css")
	if status != http.StatusOK {
		t.Fatalf("status: got %d", status)
	}
	if !strings.Contains(body, "--accent") {
		t.Fatalf("body missing CSS var: %s", body[:200])
	}
}

func TestSSEEmitsInitialFragmentBatch(t *testing.T) {
	url, _, _, stop := boot(t, []issue.Issue{mkIssue("a", "MT-1")})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/dashboard/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type: got %q", got)
	}

	r := bufio.NewReader(resp.Body)
	fragments := waitForFragmentEvents(t, r, 3, 3*time.Second)
	if len(fragments) < 3 {
		t.Fatalf("got %d fragment events, want >= 3", len(fragments))
	}
	selectors := map[string]bool{}
	for _, f := range fragments {
		selectors[f.selector] = true
	}
	for _, want := range []string{"#running", "#retrying", "#totals"} {
		if !selectors[want] {
			t.Fatalf("missing selector %q in fragments: %v", want, selectors)
		}
	}
}

type sseFragment struct {
	selector string
	body     string
}

func waitForFragmentEvents(t *testing.T, r *bufio.Reader, want int, timeout time.Duration) []sseFragment {
	t.Helper()
	type pending struct {
		event    string
		selector string
		fragment strings.Builder
	}
	out := []sseFragment{}
	cur := pending{}
	deadline := time.Now().Add(timeout)
	for len(out) < want {
		if time.Now().After(deadline) {
			t.Fatalf("timeout collecting SSE fragments; got %d want %d", len(out), want)
		}
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if cur.event == "datastar-merge-fragments" && cur.selector != "" {
				out = append(out, sseFragment{selector: cur.selector, body: cur.fragment.String()})
			}
			cur = pending{}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "event:"); ok {
			cur.event = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			rest = strings.TrimSpace(rest)
			if sel, ok := strings.CutPrefix(rest, "selector "); ok {
				cur.selector = strings.TrimSpace(sel)
				continue
			}
			if frag, ok := strings.CutPrefix(rest, "fragments "); ok {
				if cur.fragment.Len() > 0 {
					cur.fragment.WriteByte('\n')
				}
				cur.fragment.WriteString(frag)
				continue
			}
			// mergeMode etc — ignored by this test.
		}
	}
	return out
}
