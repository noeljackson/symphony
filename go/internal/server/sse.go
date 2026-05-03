package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/noeljackson/symphony/go/internal/orchestrator"
)

// serveSSE handles `GET /api/v1/events` per SPEC v3 §13.7.4. It emits an
// initial `event: snapshot` carrying the same shape as `/api/v1/state`,
// then streams one event per `EventBroadcast` until the client
// disconnects or the orchestrator shuts down (broadcast channel closes).
//
// Drop-on-backpressure: when an SSE client falls behind, the orchestrator
// sends are non-blocking — events are dropped rather than blocking the
// actor. The client re-snapshots via `/api/v1/state` to recover.
func serveSSE(w http.ResponseWriter, r *http.Request, handle *orchestrator.Handle) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_flusher", "response writer doesn't support flushing")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering for proxies

	// Initial snapshot so the client can render before any events arrive.
	if snap, ok := handle.Snapshot(r.Context()); ok {
		view := projectStateView(snap)
		body, err := json.Marshal(view)
		if err == nil {
			writeSSE(w, "snapshot", string(body))
			flusher.Flush()
		}
	}

	events, cancel := handle.SubscribeEvents()
	defer cancel()

	// Keep-alive ticker so flaky proxies don't close the stream during quiet periods.
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case ev, open := <-events:
			if !open {
				return
			}
			payload := map[string]any{
				"issue_id":         ev.IssueID,
				"issue_identifier": ev.Identifier,
				"timestamp":        time.Now().UTC().Format(time.RFC3339),
				"event":            ev.Event.Event,
				"message":          ev.Event.Message,
			}
			body, err := json.Marshal(payload)
			if err != nil {
				continue
			}
			writeSSE(w, ev.Event.Event, string(body))
			flusher.Flush()
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeSSE(w http.ResponseWriter, event, data string) {
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}
