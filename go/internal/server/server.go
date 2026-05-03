// Package server hosts the SPEC §13.7 OPTIONAL HTTP server: JSON snapshot
// at `/api/v1/state`, per-issue view at `/api/v1/<id>`, manual refresh at
// `/api/v1/refresh`, and SSE event stream at `/api/v1/events`.
//
// One HTTPServer instance owns one orchestrator handle; all routes are
// loopback-by-default (the caller picks the bind address).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/state"
)

// HTTPServer is a thin wrapper over [http.Server] that knows how to route
// the SPEC v3 §13.7 endpoints against an orchestrator [Handle].
type HTTPServer struct {
	srv      *http.Server
	listener net.Listener
}

// New constructs a server bound to addr. Use addr=":0" for an ephemeral
// port; the actual port is discoverable via [HTTPServer.Addr] after Start.
func New(addr string, handle *orchestrator.Handle) (*HTTPServer, error) {
	mux := http.NewServeMux()
	registerRoutes(mux, handle)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return &HTTPServer{srv: srv, listener: ln}, nil
}

// Addr returns the bind address (useful when the caller passed addr=":0").
func (s *HTTPServer) Addr() string { return s.listener.Addr().String() }

// Start blocks the caller serving requests. It returns nil when Shutdown
// is called, or the underlying http.Serve error otherwise.
func (s *HTTPServer) Start() error {
	if err := s.srv.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops the server and waits up to 5 seconds for in-flight
// requests to complete. SSE subscribers are closed via the orchestrator's
// shutdown — the server itself just stops accepting new ones.
func (s *HTTPServer) Shutdown(ctx context.Context) error {
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(deadline)
}

// stateView is the JSON shape for `GET /api/v1/state` (SPEC §13.7.2).
type stateView struct {
	GeneratedAt string         `json:"generated_at"`
	Counts      countsView     `json:"counts"`
	Running     []runningRow   `json:"running"`
	Retrying    []retryRow     `json:"retrying"`
	AgentTotals agentTotalView `json:"agent_totals"`
}

type countsView struct {
	Running  int `json:"running"`
	Retrying int `json:"retrying"`
}

type runningRow struct {
	IssueID         string            `json:"issue_id"`
	IssueIdentifier string            `json:"issue_identifier"`
	State           string            `json:"state"`
	SessionID       string            `json:"session_id,omitempty"`
	TurnCount       uint32            `json:"turn_count"`
	LastEvent       string            `json:"last_event,omitempty"`
	LastMessage     string            `json:"last_message,omitempty"`
	StartedAt       string            `json:"started_at"`
	LastEventAt     string            `json:"last_event_at,omitempty"`
	Tokens          tokenView         `json:"tokens"`
	RecentEvents    []recentEventView `json:"recent_events"`
}

type retryRow struct {
	IssueID         string `json:"issue_id"`
	IssueIdentifier string `json:"issue_identifier"`
	Attempt         uint32 `json:"attempt"`
	DueAt           string `json:"due_at"`
	Error           string `json:"error,omitempty"`
}

type tokenView struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	TotalTokens  uint64 `json:"total_tokens"`
}

type agentTotalView struct {
	InputTokens    uint64   `json:"input_tokens"`
	OutputTokens   uint64   `json:"output_tokens"`
	TotalTokens    uint64   `json:"total_tokens"`
	SecondsRunning float64  `json:"seconds_running"`
	CostUSD        *float64 `json:"cost_usd"`
	CostUSDToday   *float64 `json:"cost_usd_today"`
}

type recentEventView struct {
	At      string `json:"at"`
	Event   string `json:"event"`
	Message string `json:"message,omitempty"`
}

type issueView struct {
	IssueIdentifier string            `json:"issue_identifier"`
	IssueID         string            `json:"issue_id"`
	Status          string            `json:"status"`
	Running         *runningRow       `json:"running"`
	Retry           *retryRow         `json:"retry"`
	RecentEvents    []recentEventView `json:"recent_events"`
}

type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type refreshResponse struct {
	Queued      bool     `json:"queued"`
	Coalesced   bool     `json:"coalesced"`
	RequestedAt string   `json:"requested_at"`
	Operations  []string `json:"operations"`
}

func registerRoutes(mux *http.ServeMux, handle *orchestrator.Handle) {
	mux.HandleFunc("/api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		snap, ok := handle.Snapshot(r.Context())
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "snapshot_unavailable",
				"orchestrator not responding")
			return
		}
		writeJSON(w, http.StatusOK, projectStateView(snap))
	})

	mux.HandleFunc("/api/v1/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		handle.Tick(r.Context())
		writeJSON(w, http.StatusAccepted, refreshResponse{
			Queued:      true,
			Coalesced:   false,
			RequestedAt: time.Now().UTC().Format(time.RFC3339),
			Operations:  []string{"poll", "reconcile"},
		})
	})

	mux.HandleFunc("/api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		serveSSE(w, r, handle)
	})

	// /api/v1/<identifier> — must come last because it overlaps state/refresh/events.
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
		// Reserved subpaths handled by the more specific routes above.
		if path == "" || path == "state" || path == "refresh" || path == "events" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		serveIssueView(w, r, handle, path)
	})
}

func serveIssueView(w http.ResponseWriter, r *http.Request, handle *orchestrator.Handle, identifier string) {
	snap, ok := handle.Snapshot(r.Context())
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "snapshot_unavailable",
			"orchestrator not responding")
		return
	}
	view, found := projectIssueView(snap, identifier)
	if !found {
		writeError(w, http.StatusNotFound, "issue_not_found",
			fmt.Sprintf("issue %q is not currently tracked", identifier))
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func projectIssueView(snap orchestrator.Snapshot, identifier string) (*issueView, bool) {
	for _, r := range snap.Running {
		if strings.EqualFold(r.Identifier, identifier) {
			row := runningRowFromSnapshot(r)
			recent := projectRecent(r.RecentEvents)
			return &issueView{
				IssueIdentifier: r.Identifier,
				IssueID:         r.IssueID,
				Status:          "running",
				Running:         &row,
				Retry:           nil,
				RecentEvents:    recent,
			}, true
		}
	}
	for _, r := range snap.Retrying {
		if strings.EqualFold(r.Identifier, identifier) {
			row := retryRowFromSnapshot(r)
			return &issueView{
				IssueIdentifier: r.Identifier,
				IssueID:         r.IssueID,
				Status:          "retrying",
				Running:         nil,
				Retry:           &row,
				RecentEvents:    []recentEventView{},
			}, true
		}
	}
	return nil, false
}

func projectStateView(snap orchestrator.Snapshot) stateView {
	running := make([]runningRow, 0, len(snap.Running))
	for _, r := range snap.Running {
		running = append(running, runningRowFromSnapshot(r))
	}
	retrying := make([]retryRow, 0, len(snap.Retrying))
	for _, r := range snap.Retrying {
		retrying = append(retrying, retryRowFromSnapshot(r))
	}
	return stateView{
		GeneratedAt: snap.GeneratedAt.Format(time.RFC3339),
		Counts:      countsView{Running: len(running), Retrying: len(retrying)},
		Running:     running,
		Retrying:    retrying,
		AgentTotals: agentTotalView{
			InputTokens:    snap.AgentTotals.InputTokens,
			OutputTokens:   snap.AgentTotals.OutputTokens,
			TotalTokens:    snap.AgentTotals.TotalTokens,
			SecondsRunning: snap.AgentTotals.SecondsRunning,
			CostUSD:        snap.AgentTotals.CostUSD,
			CostUSDToday:   snap.AgentTotals.CostUSDToday,
		},
	}
}

func runningRowFromSnapshot(r orchestrator.SnapshotRunningRow) runningRow {
	out := runningRow{
		IssueID:         r.IssueID,
		IssueIdentifier: r.Identifier,
		State:           r.State,
		SessionID:       r.SessionID,
		TurnCount:       r.TurnCount,
		LastEvent:       r.LastEvent,
		LastMessage:     r.LastMessage,
		StartedAt:       r.StartedAt.Format(time.RFC3339),
		Tokens: tokenView{
			InputTokens:  r.InputTokens,
			OutputTokens: r.OutputTokens,
			TotalTokens:  r.TotalTokens,
		},
		RecentEvents: projectRecent(r.RecentEvents),
	}
	if r.LastEventAt != nil {
		out.LastEventAt = r.LastEventAt.Format(time.RFC3339)
	}
	return out
}

func retryRowFromSnapshot(r orchestrator.SnapshotRetryRow) retryRow {
	due := time.Now().Add(time.Duration(r.DueInMS) * time.Millisecond)
	return retryRow{
		IssueID:         r.IssueID,
		IssueIdentifier: r.Identifier,
		Attempt:         r.Attempt,
		DueAt:           due.UTC().Format(time.RFC3339),
		Error:           r.Error,
	}
}

func projectRecent(in []state.RecentEvent) []recentEventView {
	out := make([]recentEventView, 0, len(in))
	for _, e := range in {
		out = append(out, recentEventView{
			At:      e.At.UTC().Format(time.RFC3339),
			Event:   e.Event,
			Message: e.Message,
		})
	}
	return out
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Error: apiErrorBody{Code: code, Message: message}})
}
