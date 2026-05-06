// Package logsclient implements the `symphony logs <identifier>` flow per
// SPEC v3 §13.7.2 + §18.2.
//
//  1. GET <url>/api/v1/<identifier> for backfill (recent_events array).
//  2. With Follow=true (default), subscribe to <url>/api/v1/events and
//     print events whose `issue_identifier` matches.
//  3. With Follow=false, print backfill and exit 0.
//
// 404 + empty backfill exits 1 ("issue not tracked"). Bad URL or
// unreachable host exits 2.
package logsclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Args bundles the run-time parameters.
type Args struct {
	Identifier string
	URL        string
	Follow     bool
	HTTPClient *http.Client // optional; defaults to a 10s GET / no-timeout SSE client.
}

// Run prints backfill (and optionally tails the SSE stream) to out. Returns
// the process exit code.
func Run(ctx context.Context, args Args, out io.Writer, errOut io.Writer) (int, error) {
	if err := ParseURL(args.URL); err != nil {
		fmt.Fprintf(errOut, "symphony logs: %v\n", err)
		return 2, nil
	}
	base := strings.TrimRight(args.URL, "/")
	issueURL := fmt.Sprintf("%s/api/v1/%s", base, args.Identifier)

	hc := args.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issueURL, nil)
	if err != nil {
		fmt.Fprintf(errOut, "symphony logs: %v\n", err)
		return 2, nil
	}
	resp, err := hc.Do(req)
	if err != nil {
		fmt.Fprintf(errOut, "symphony logs: connect to %s: %v\n", issueURL, err)
		return 2, nil
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		_ = resp.Body.Close()
		fmt.Fprintf(errOut, "symphony logs: issue not tracked: %s\n", args.Identifier)
		return 1, nil
	default:
		_ = resp.Body.Close()
		fmt.Fprintf(errOut, "symphony logs: unexpected status %d from %s\n", resp.StatusCode, issueURL)
		return 2, nil
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		fmt.Fprintf(errOut, "symphony logs: read %s: %v\n", issueURL, err)
		return 2, nil
	}
	var view issueResponse
	if err := json.Unmarshal(body, &view); err != nil {
		fmt.Fprintf(errOut, "symphony logs: parse %s: %v\n", issueURL, err)
		return 2, nil
	}
	for _, ev := range view.RecentEvents {
		writeLine(out, ev.At, ev.Event, ev.Message)
	}
	if !args.Follow {
		return 0, nil
	}

	// Live tail. SSE clients shouldn't have a body-read timeout, so we
	// build a fresh client without one for this leg.
	streamClient := *hc
	streamClient.Timeout = 0
	streamURL := base + "/api/v1/events"
	streamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		fmt.Fprintf(errOut, "symphony logs: SSE: %v\n", err)
		return 0, nil
	}
	streamReq.Header.Set("Accept", "text/event-stream")
	streamResp, err := streamClient.Do(streamReq)
	if err != nil {
		fmt.Fprintf(errOut, "symphony logs: SSE failed: %v\n", err)
		return 0, nil
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		fmt.Fprintf(errOut, "symphony logs: SSE status %d from %s\n", streamResp.StatusCode, streamURL)
		return 0, nil
	}

	if err := tailSSE(streamResp.Body, args.Identifier, out); err != nil && !errors.Is(err, io.EOF) {
		// ctx cancellations are normal Ctrl-C exits.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(errOut, "symphony logs: SSE stream: %v\n", err)
		}
	}
	return 0, nil
}

type issueResponse struct {
	RecentEvents []recentEvent `json:"recent_events"`
}

type recentEvent struct {
	At      string `json:"at"`
	Event   string `json:"event"`
	Message string `json:"message"`
}

// tailSSE reads `text/event-stream` frames from r and prints those whose
// data payload's `issue_identifier` matches `wanted`. Returns when the
// stream closes or context is cancelled (ctx threading happens through
// http.NewRequestWithContext above).
func tailSSE(r io.Reader, wanted string, out io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var (
		event string
		data  strings.Builder
	)
	flush := func() {
		if data.Len() == 0 {
			event = ""
			data.Reset()
			return
		}
		if event == "snapshot" || event == "lagged" {
			event = ""
			data.Reset()
			return
		}
		var parsed sseFrame
		if err := json.Unmarshal([]byte(data.String()), &parsed); err == nil {
			if parsed.IssueIdentifier == wanted {
				at := parsed.Timestamp
				kind := parsed.Event
				if kind == "" {
					kind = event
				}
				writeLine(out, at, kind, parsed.Message)
			}
		}
		event = ""
		data.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			// Comment / keep-alive — ignore.
			continue
		}
		if v, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(v)
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(v))
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	flush()
	return nil
}

type sseFrame struct {
	IssueIdentifier string `json:"issue_identifier"`
	Timestamp       string `json:"timestamp"`
	Event           string `json:"event"`
	Message         string `json:"message"`
}

func writeLine(out io.Writer, at, event, message string) {
	if message == "" {
		fmt.Fprintf(out, "%s  %s\n", at, event)
		return
	}
	fmt.Fprintf(out, "%s  %s  %s\n", at, event, message)
}

// ParseURL validates that the URL has http/https scheme and a non-empty
// host. Used by the CLI before any network I/O so bad URLs surface as
// exit-2 with a clear message instead of a generic connect error.
func ParseURL(raw string) error {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("--url must start with http:// or https://: %q", raw)
	}
	if strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://") == "" {
		return fmt.Errorf("--url is missing host: %q", raw)
	}
	return nil
}
