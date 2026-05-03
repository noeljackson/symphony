package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/noeljackson/symphony/go/internal/tracker/github"
)

func fakeIssues(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			http.Error(w, "missing accept", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestNewRejectsMissingOwner(t *testing.T) {
	_, err := github.New(github.Config{Repo: "r", APIToken: "t"})
	if err == nil {
		t.Fatal("expected error for missing Owner")
	}
}

func TestNewRejectsMissingRepo(t *testing.T) {
	_, err := github.New(github.Config{Owner: "o", APIToken: "t"})
	if err == nil {
		t.Fatal("expected error for missing Repo")
	}
}

func TestNewRejectsMissingAPIToken(t *testing.T) {
	_, err := github.New(github.Config{Owner: "o", Repo: "r"})
	if err == nil {
		t.Fatal("expected error for missing APIToken")
	}
}

func TestFetchCandidateIssuesFiltersByLabel(t *testing.T) {
	srv := fakeIssues(t, `[
		{"number":1,"title":"alpha","state":"open","labels":[{"name":"ready"}],"node_id":"a"},
		{"number":2,"title":"beta","state":"open","labels":[{"name":"backlog"}],"node_id":"b"},
		{"number":3,"title":"gamma","state":"closed","labels":[{"name":"done"}],"node_id":"c"}
	]`)
	defer srv.Close()

	c, err := github.New(github.Config{
		Endpoint:       srv.URL,
		Owner:          "noeljackson",
		Repo:           "symphony",
		APIToken:       "test-token",
		ActiveStates:   []string{"ready", "in-progress"},
		TerminalStates: []string{"done", "closed"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d issues, want 1", len(out))
	}
	if out[0].Identifier != "noeljackson/symphony#1" {
		t.Fatalf("identifier: got %q", out[0].Identifier)
	}
	if out[0].State != "ready" {
		t.Fatalf("state: got %q want ready", out[0].State)
	}
	if out[0].BranchName == nil || *out[0].BranchName != "1-alpha" {
		t.Fatalf("branch: got %v want 1-alpha", out[0].BranchName)
	}
}

func TestClosedIssueIsTerminalEvenWithoutLabel(t *testing.T) {
	srv := fakeIssues(t, `[
		{"number":7,"title":"x","state":"closed","labels":[],"node_id":"a"}
	]`)
	defer srv.Close()
	c, _ := github.New(github.Config{
		Endpoint: srv.URL, Owner: "o", Repo: "r", APIToken: "test-token",
		ActiveStates:   []string{"ready"},
		TerminalStates: []string{"done"},
	})
	out, err := c.FetchIssuesByStates(context.Background(), []string{"closed"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}
	if len(out) != 1 || out[0].State != "closed" {
		t.Fatalf("got %+v want one closed issue", out)
	}
}

func TestPullRequestsAreFiltered(t *testing.T) {
	srv := fakeIssues(t, `[
		{"number":1,"title":"real","state":"open","labels":[{"name":"ready"}],"node_id":"a"},
		{"number":2,"title":"pr","state":"open","labels":[{"name":"ready"}],"pull_request":{"url":"x"}}
	]`)
	defer srv.Close()
	c, _ := github.New(github.Config{
		Endpoint: srv.URL, Owner: "o", Repo: "r", APIToken: "test-token",
		ActiveStates: []string{"ready"},
	})
	out, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d want 1 (PR must be filtered)", len(out))
	}
}

func TestPriorityFromLabelsPicksLowestNumeric(t *testing.T) {
	srv := fakeIssues(t, `[
		{"number":1,"title":"x","state":"open","labels":[{"name":"ready"},{"name":"P2"},{"name":"P0"}],"node_id":"a"}
	]`)
	defer srv.Close()
	c, _ := github.New(github.Config{
		Endpoint: srv.URL, Owner: "o", Repo: "r", APIToken: "test-token",
		ActiveStates:     []string{"ready"},
		LabelPriorityMap: map[string]int{"P0": 0, "P1": 1, "P2": 2},
	})
	out, _ := c.FetchCandidateIssues(context.Background())
	if len(out) != 1 || out[0].Priority == nil || *out[0].Priority != 0 {
		t.Fatalf("priority: got %+v want 0", out[0].Priority)
	}
}

func TestPriorityNilWhenNoLabelMatches(t *testing.T) {
	srv := fakeIssues(t, `[
		{"number":1,"title":"x","state":"open","labels":[{"name":"ready"}],"node_id":"a"}
	]`)
	defer srv.Close()
	c, _ := github.New(github.Config{
		Endpoint: srv.URL, Owner: "o", Repo: "r", APIToken: "test-token",
		ActiveStates:     []string{"ready"},
		LabelPriorityMap: map[string]int{"P0": 0},
	})
	out, _ := c.FetchCandidateIssues(context.Background())
	if out[0].Priority != nil {
		t.Fatalf("priority: got %v want nil", *out[0].Priority)
	}
}

func TestAssigneeFilterPropagatesToQuery(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c, _ := github.New(github.Config{
		Endpoint: srv.URL, Owner: "o", Repo: "r", APIToken: "t", Assignee: "noeljackson",
	})
	if _, err := c.FetchCandidateIssues(context.Background()); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	q := <-got
	if !strings.Contains(q, "assignee=noeljackson") {
		t.Fatalf("query missing assignee filter: %s", q)
	}
	if !strings.Contains(q, "state=all") {
		t.Fatalf("expected state=all in query, got: %s", q)
	}
}

func TestFetchSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()
	c, _ := github.New(github.Config{
		Endpoint: srv.URL, Owner: "o", Repo: "r", APIToken: "t",
	})
	_, err := c.FetchCandidateIssues(context.Background())
	if err == nil || !strings.Contains(err.Error(), "github http 401") {
		t.Fatalf("expected http 401 error, got %v", err)
	}
}

func TestBranchSlugTruncatesAndStripsSpecials(t *testing.T) {
	srv := fakeIssues(t, `[
		{"number":42,"title":"  Fix \"Login\" Flow !! And other things that go on for quite a while ","state":"open","labels":[{"name":"ready"}],"node_id":"a"}
	]`)
	defer srv.Close()
	c, _ := github.New(github.Config{
		Endpoint: srv.URL, Owner: "o", Repo: "r", APIToken: "test-token",
		ActiveStates: []string{"ready"},
	})
	out, _ := c.FetchCandidateIssues(context.Background())
	if len(out) != 1 {
		t.Fatalf("got %d want 1", len(out))
	}
	br := *out[0].BranchName
	if !strings.HasPrefix(br, "42-") {
		t.Fatalf("branch prefix: got %q want 42-…", br)
	}
	if strings.Contains(br, " ") || strings.Contains(br, "\"") || strings.Contains(br, "!") {
		t.Fatalf("branch has unsanitized chars: %q", br)
	}
	if strings.Contains(br, "--") {
		t.Fatalf("branch has consecutive dashes: %q", br)
	}
}
