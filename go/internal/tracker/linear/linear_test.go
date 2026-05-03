package linear_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/noeljackson/symphony/go/internal/tracker/linear"
)

// fakeServer returns an httptest.Server that responds to every POST with
// the given JSON body. Caller owns the Close.
func fakeServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("Authorization"); got != "test-key" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		// Drain body so the client doesn't see ECONNRESET.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestNewRejectsMissingAPIKey(t *testing.T) {
	_, err := linear.New(linear.Config{ProjectSlug: "demo"})
	if err == nil {
		t.Fatal("expected error for missing APIKey")
	}
}

func TestNewRejectsMissingProjectSlug(t *testing.T) {
	_, err := linear.New(linear.Config{APIKey: "k"})
	if err == nil {
		t.Fatal("expected error for missing ProjectSlug")
	}
}

func TestFetchCandidateIssuesFiltersByActiveStates(t *testing.T) {
	srv := fakeServer(t, `{
		"data": {
			"project": {
				"issues": {
					"nodes": [
						{"id":"a","identifier":"MT-1","title":"alpha","priority":1,"state":{"name":"Todo"}},
						{"id":"b","identifier":"MT-2","title":"beta","priority":2,"state":{"name":"In Progress"}},
						{"id":"c","identifier":"MT-3","title":"gamma","priority":3,"state":{"name":"Done"}}
					]
				}
			}
		}
	}`)
	defer srv.Close()

	c, err := linear.New(linear.Config{
		Endpoint:       srv.URL,
		APIKey:         "test-key",
		ProjectSlug:    "demo",
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d issues, want 2", len(out))
	}
	idents := []string{out[0].Identifier, out[1].Identifier}
	for _, id := range idents {
		if id != "MT-1" && id != "MT-2" {
			t.Fatalf("unexpected identifier %q in %v", id, idents)
		}
	}
}

func TestFetchIssueStatesByIDsReturnsRequested(t *testing.T) {
	srv := fakeServer(t, `{
		"data": {
			"project": {
				"issues": {
					"nodes": [
						{"id":"a","identifier":"MT-1","title":"a","state":{"name":"Todo"}},
						{"id":"b","identifier":"MT-2","title":"b","state":{"name":"In Progress"}}
					]
				}
			}
		}
	}`)
	defer srv.Close()

	c, _ := linear.New(linear.Config{
		Endpoint: srv.URL, APIKey: "test-key", ProjectSlug: "demo",
		ActiveStates: []string{"Todo", "In Progress"},
	})
	out, err := c.FetchIssueStatesByIDs(context.Background(), []string{"b", "missing"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if len(out) != 1 || out[0].Identifier != "MT-2" {
		t.Fatalf("got %+v want 1 issue MT-2", out)
	}
}

func TestFetchIssuesByStatesUsesProvidedFilter(t *testing.T) {
	srv := fakeServer(t, `{
		"data": {
			"project": {
				"issues": {
					"nodes": [
						{"id":"a","identifier":"MT-1","title":"a","state":{"name":"Done"}},
						{"id":"b","identifier":"MT-2","title":"b","state":{"name":"Todo"}}
					]
				}
			}
		}
	}`)
	defer srv.Close()

	c, _ := linear.New(linear.Config{
		Endpoint: srv.URL, APIKey: "test-key", ProjectSlug: "demo",
	})
	out, err := c.FetchIssuesByStates(context.Background(), []string{"Done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}
	if len(out) != 1 || out[0].Identifier != "MT-1" {
		t.Fatalf("got %+v want 1 issue MT-1", out)
	}
}

func TestProjectionMapsBlockersAndLabels(t *testing.T) {
	srv := fakeServer(t, `{
		"data": {
			"project": {
				"issues": {
					"nodes": [{
						"id":"a","identifier":"MT-1","title":"alpha",
						"priority":2,"state":{"name":"Todo"},
						"labels":{"nodes":[{"name":"P1"},{"name":"backend"}]},
						"inverseRelations":{"nodes":[
							{"type":"blocks","issue":{"id":"x","identifier":"MT-X","state":{"name":"In Progress"}}},
							{"type":"duplicate","issue":{"id":"y","identifier":"MT-Y","state":{"name":"Done"}}}
						]}
					}]
				}
			}
		}
	}`)
	defer srv.Close()
	c, _ := linear.New(linear.Config{
		Endpoint: srv.URL, APIKey: "test-key", ProjectSlug: "demo",
		ActiveStates: []string{"Todo"},
	})
	out, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d want 1", len(out))
	}
	got := out[0]
	if got.Priority == nil || *got.Priority != 2 {
		t.Fatalf("priority: got %v want 2", got.Priority)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "P1" {
		t.Fatalf("labels: got %v", got.Labels)
	}
	// Only the `blocks` relation produces a Blocker; `duplicate` is filtered.
	if len(got.BlockedBy) != 1 || got.BlockedBy[0].Identifier != "MT-X" {
		t.Fatalf("blockers: got %+v want 1 entry MT-X", got.BlockedBy)
	}
}

func TestFetchSurfacesGraphqlErrors(t *testing.T) {
	srv := fakeServer(t, `{"errors":[{"message":"project not found"}]}`)
	defer srv.Close()
	c, _ := linear.New(linear.Config{
		Endpoint: srv.URL, APIKey: "test-key", ProjectSlug: "demo",
	})
	_, err := c.FetchCandidateIssues(context.Background())
	if err == nil || !strings.Contains(err.Error(), "project not found") {
		t.Fatalf("expected project-not-found error, got %v", err)
	}
}

func TestFetchSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server boom"))
	}))
	defer srv.Close()
	c, _ := linear.New(linear.Config{
		Endpoint: srv.URL, APIKey: "test-key", ProjectSlug: "demo",
	})
	_, err := c.FetchCandidateIssues(context.Background())
	if err == nil || !strings.Contains(err.Error(), "linear http 500") {
		t.Fatalf("expected http 500 error, got %v", err)
	}
}

func TestFetchSendsExpectedQueryAndAuth(t *testing.T) {
	type capturedReq struct {
		Auth string
		Body map[string]any
	}
	captured := make(chan capturedReq, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		captured <- capturedReq{Auth: r.Header.Get("Authorization"), Body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"project":{"issues":{"nodes":[]}}}}`))
	}))
	defer srv.Close()

	c, _ := linear.New(linear.Config{
		Endpoint: srv.URL, APIKey: "k", ProjectSlug: "demo", PageSize: 99,
	})
	if _, err := c.FetchCandidateIssues(context.Background()); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got := <-captured
	if got.Auth != "k" {
		t.Fatalf("auth: got %q want %q", got.Auth, "k")
	}
	vars, _ := got.Body["variables"].(map[string]any)
	if vars == nil {
		t.Fatalf("missing variables in request body: %+v", got.Body)
	}
	if vars["slug"] != "demo" {
		t.Fatalf("slug: got %v want demo", vars["slug"])
	}
	if int(vars["first"].(float64)) != 99 {
		t.Fatalf("page size: got %v want 99", vars["first"])
	}
	q, _ := got.Body["query"].(string)
	if !strings.Contains(q, "SymphonyProjectIssues") {
		t.Fatalf("query missing operation name; got: %s", q)
	}
}
