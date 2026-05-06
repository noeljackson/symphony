package logsclient_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/logsclient"
)

func TestParseURL(t *testing.T) {
	for _, ok := range []string{"http://localhost:8080", "https://example.test"} {
		if err := logsclient.ParseURL(ok); err != nil {
			t.Fatalf("expected ok for %q, got %v", ok, err)
		}
	}
	for _, bad := range []string{"ftp://x", "localhost:8080", "http://", ""} {
		if err := logsclient.ParseURL(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestRunRejectsBadURL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := logsclient.Run(context.Background(), logsclient.Args{
		Identifier: "MT-1",
		URL:        "localhost:8080",
		Follow:     false,
	}, &stdout, &stderr)
	if err != nil || code != 2 {
		t.Fatalf("got code=%d err=%v want code=2", code, err)
	}
	if !strings.Contains(stderr.String(), "must start with http") {
		t.Fatalf("stderr: %s", stderr.String())
	}
}

func TestRun404ReturnsExit1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"issue_not_found","message":"x"}}`))
	}))
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	code, _ := logsclient.Run(context.Background(), logsclient.Args{
		Identifier: "MT-MISSING", URL: srv.URL, Follow: false,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code: got %d want 1", code)
	}
	if !strings.Contains(stderr.String(), "issue not tracked") {
		t.Fatalf("stderr: %s", stderr.String())
	}
}

func TestRunPrintsBackfillAndExitsOnNoFollow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"issue_identifier":"MT-1","issue_id":"a","status":"running",
			"recent_events":[
				{"at":"2026-05-06T13:00:00Z","event":"session_started","message":"MT-1"},
				{"at":"2026-05-06T13:00:01Z","event":"turn_completed","message":"usage in=10 out=5"}
			]
		}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code, _ := logsclient.Run(context.Background(), logsclient.Args{
		Identifier: "MT-1", URL: srv.URL, Follow: false,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code: got %d want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "session_started") || !strings.Contains(out, "turn_completed") {
		t.Fatalf("stdout missing backfill events: %s", out)
	}
	if !strings.Contains(out, "usage in=10 out=5") {
		t.Fatalf("stdout missing message: %s", out)
	}
}

func TestRunFollowsSSEAndFiltersByIdentifier(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/MT-1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issue_identifier":"MT-1","issue_id":"a","status":"running","recent_events":[]}`))
	})
	mux.HandleFunc("/api/v1/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// Drop a snapshot frame (must be filtered out).
		fmt.Fprint(w, "event: snapshot\ndata: {}\n\n")
		flusher.Flush()
		// One MT-2 event (not what we want).
		fmt.Fprint(w, "event: assistant_message\n")
		fmt.Fprint(w, `data: {"issue_id":"b","issue_identifier":"MT-2","timestamp":"t","event":"assistant_message","message":"other"}`+"\n\n")
		flusher.Flush()
		// One MT-1 event (the target).
		fmt.Fprint(w, "event: turn_completed\n")
		fmt.Fprint(w, `data: {"issue_id":"a","issue_identifier":"MT-1","timestamp":"t","event":"turn_completed","message":"usage in=2 out=1"}`+"\n\n")
		flusher.Flush()
		// Close the stream so Run returns.
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code, _ := logsclient.Run(ctx, logsclient.Args{
		Identifier: "MT-1", URL: srv.URL, Follow: true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code: got %d want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "turn_completed") || !strings.Contains(out, "in=2 out=1") {
		t.Fatalf("stdout missing matching event: %q", out)
	}
	if strings.Contains(out, "MT-2") || strings.Contains(out, "other") {
		t.Fatalf("stdout leaked non-matching events: %q", out)
	}
}
