package watcher_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/watcher"
)

func writeWorkflow(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

const goodWorkflow = `---
tracker:
  kind: linear
linear:
  api_key: test-key
  project_slug: demo
agent:
  backend: codex
codex:
  command: codex app-server
---
Issue {{ issue.identifier }}
`

const updatedWorkflow = `---
tracker:
  kind: linear
linear:
  api_key: test-key
  project_slug: updated-project
polling:
  interval_ms: 7777
agent:
  backend: codex
codex:
  command: codex app-server
---
Issue {{ issue.identifier }} (updated)
`

const invalidWorkflow = `---
tracker:
  kind: linear
linear:
  project_slug: demo
agent:
  backend: codex
codex:
  command: codex app-server
---
body
`

func TestWatchEmitsReloadOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, goodWorkflow)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := watcher.Watch(ctx, path)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Edit the file.
	writeWorkflow(t, path, updatedWorkflow)

	select {
	case ev := <-events:
		if ev.Err != nil {
			t.Fatalf("err: %v", ev.Err)
		}
		if ev.Definition == nil {
			t.Fatal("nil definition")
		}
		if ev.Definition.Config.Polling.IntervalMS != 7777 {
			t.Fatalf("poll interval: got %d want 7777", ev.Definition.Config.Polling.IntervalMS)
		}
		if ev.Definition.Config.Linear.ProjectSlug != "updated-project" {
			t.Fatalf("project: got %q want updated-project", ev.Definition.Config.Linear.ProjectSlug)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for reload event")
	}
}

func TestWatchEmitsErrOnInvalidReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, goodWorkflow)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := watcher.Watch(ctx, path)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Replace with a workflow that fails ValidateForDispatch.
	writeWorkflow(t, path, invalidWorkflow)

	select {
	case ev := <-events:
		if ev.Err == nil {
			t.Fatal("expected err")
		}
		if !strings.Contains(ev.Err.Error(), "validate") {
			t.Fatalf("err: %v want substring 'validate'", ev.Err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for err event")
	}
}

func TestWatchDebouncesBurstWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, goodWorkflow)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := watcher.Watch(ctx, path)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Fire multiple writes within the debounce window.
	for i := 0; i < 5; i++ {
		writeWorkflow(t, path, updatedWorkflow)
		time.Sleep(20 * time.Millisecond)
	}

	// First event lands within ~debounce-window after the last write.
	select {
	case ev := <-events:
		if ev.Err != nil {
			t.Fatalf("first event err: %v", ev.Err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for first event")
	}

	// No further events should arrive within a short follow-up window
	// since there were no further file writes.
	select {
	case ev, ok := <-events:
		if ok {
			t.Fatalf("unexpected second event: %+v", ev)
		}
	case <-time.After(2 * watcher.DebounceWindow):
	}
}

func TestWatchEmitsErrOnMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, goodWorkflow)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := watcher.Watch(ctx, path)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Remove the file. Watcher should observe Remove + emit a reload err.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Err == nil {
			t.Fatal("expected err on missing file")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for err event")
	}
}

func TestWatchClosesChannelOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, goodWorkflow)

	ctx, cancel := context.WithCancel(context.Background())
	events, err := watcher.Watch(ctx, path)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return // closed; success
			}
		case <-deadline.C:
			t.Fatal("watcher channel never closed after ctx cancel")
		}
	}
}
