package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/noeljackson/symphony/go/internal/orchestrator"
)

// Mount registers the dashboard routes on `mux`:
//   - GET /            → server-rendered HTML page
//   - GET /dashboard.css → static stylesheet
//   - GET /dashboard/sse → Datastar fragment-merge stream
//
// The handle is a clone of the orchestrator's public surface; the
// dashboard never mutates state, only reads snapshots and broadcasts.
func Mount(mux *http.ServeMux, handle *orchestrator.Handle) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snap, ok := handle.Snapshot(r.Context())
		if !ok {
			http.Error(w, "snapshot unavailable", http.StatusServiceUnavailable)
			return
		}
		body, err := RenderPage(FromSnapshot(snap))
		if err != nil {
			http.Error(w, "render error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})

	mux.HandleFunc("/dashboard.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write([]byte(CSS()))
	})

	mux.HandleFunc("/dashboard/sse", func(w http.ResponseWriter, r *http.Request) {
		serveSSE(w, r, handle)
	})
}

// serveSSE streams Datastar fragment-merge events. The first batch is
// emitted immediately with the current snapshot; subsequent batches fire
// on every orchestrator EventBroadcast (debounced to one per 250ms so
// the page doesn't churn under heavy event traffic).
func serveSSE(w http.ResponseWriter, r *http.Request, handle *orchestrator.Handle) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	emitFragments(r.Context(), w, flusher, handle)

	subs, cancel := handle.SubscribeEvents()
	defer cancel()

	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()

	pending := false
	for {
		select {
		case <-r.Context().Done():
			return
		case _, open := <-subs:
			if !open {
				return
			}
			if !pending {
				debounce.Reset(250 * time.Millisecond)
				pending = true
			}
		case <-debounce.C:
			pending = false
			emitFragments(r.Context(), w, flusher, handle)
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}

func emitFragments(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, handle *orchestrator.Handle) {
	snap, ok := handle.Snapshot(ctx)
	if !ok {
		return
	}
	view := FromSnapshotFragment(snap)
	for _, frag := range []struct {
		Name     string
		Selector string
		Render   func(FragmentView) (string, error)
	}{
		{"running", "#running", RenderRunning},
		{"retrying", "#retrying", RenderRetrying},
		{"totals", "#totals", RenderTotals},
	} {
		body, err := frag.Render(view)
		if err != nil {
			continue
		}
		writeDatastarMergeFragments(w, frag.Selector, body)
	}
	flusher.Flush()
}

// writeDatastarMergeFragments emits one `datastar-merge-fragments` SSE
// event with the standard fields (selector, mergeMode=morph, fragments).
//
// The Datastar v1 wire format is line-oriented inside the SSE `data:`
// stanzas — `selector`, `mergeMode`, and `fragments` each on their own
// data line. Multi-line fragments are emitted as repeated `data:
// fragments ...` lines (Datastar reassembles them).
func writeDatastarMergeFragments(w http.ResponseWriter, selector, fragment string) {
	fmt.Fprint(w, "event: datastar-merge-fragments\n")
	fmt.Fprintf(w, "data: selector %s\n", selector)
	fmt.Fprint(w, "data: mergeMode morph\n")
	for _, line := range strings.Split(strings.TrimRight(fragment, "\n"), "\n") {
		fmt.Fprintf(w, "data: fragments %s\n", line)
	}
	fmt.Fprint(w, "\n")
}
