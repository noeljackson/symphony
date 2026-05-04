package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/workspace"
)

func TestSanitizeIdentifier(t *testing.T) {
	cases := map[string]string{
		"MT-1":           "MT-1",
		"sym/op_v2":      "sym_op_v2",
		"some weird id!": "some_weird_id_",
		"":               "_",
		"abc.def-123":    "abc.def-123",
	}
	for in, want := range cases {
		if got := workspace.SanitizeIdentifier(in); got != want {
			t.Fatalf("Sanitize(%q): got %q want %q", in, got, want)
		}
	}
}

func TestPathForRejectsEscape(t *testing.T) {
	root := t.TempDir()
	m, err := workspace.New(root, workspace.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	// Sanitization replaces `..` with `__` so the candidate stays under
	// root; PathFor never returns an escape, but we keep ErrEscapedRoot
	// reachable via direct path manipulation in callers.
	got, err := m.PathFor("../escape")
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if !strings.HasPrefix(got, root) {
		t.Fatalf("path: got %q want under %q", got, root)
	}
}

func TestEnsureCreatedRunsAfterCreateOnce(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "marker.txt")
	hooks := workspace.Hooks{
		AfterCreate: "echo created >> " + marker,
		Timeout:     5 * time.Second,
	}
	m, err := workspace.New(root, hooks)
	if err != nil {
		t.Fatal(err)
	}
	path, created, err := m.EnsureCreated(context.Background(), "MT-1")
	if err != nil {
		t.Fatalf("first EnsureCreated: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on first call")
	}
	if !strings.HasSuffix(path, "MT-1") {
		t.Fatalf("path: got %q want suffix MT-1", path)
	}

	// Second call must NOT re-fire after_create.
	_, created2, err := m.EnsureCreated(context.Background(), "MT-1")
	if err != nil {
		t.Fatalf("second EnsureCreated: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on second call")
	}

	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if got := strings.Count(string(body), "created"); got != 1 {
		t.Fatalf("after_create fired %d times, want 1", got)
	}
}

func TestAfterCreateFailureRollsBackDirectory(t *testing.T) {
	root := t.TempDir()
	hooks := workspace.Hooks{AfterCreate: "exit 7", Timeout: 5 * time.Second}
	m, _ := workspace.New(root, hooks)
	path, _, err := m.EnsureCreated(context.Background(), "MT-1")
	if err == nil {
		t.Fatal("expected after_create failure to surface")
	}
	if path != "" {
		t.Fatalf("path: got %q want empty on failure", path)
	}
	candidate := filepath.Join(root, "MT-1")
	if _, statErr := os.Stat(candidate); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("workspace dir must be cleaned up on hook failure; stat err=%v", statErr)
	}
}

func TestRemoveFiresBeforeRemoveBestEffort(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "removed.txt")
	hooks := workspace.Hooks{
		BeforeRemove: "echo gone >> " + marker + "; exit 1", // failing hook
		Timeout:      5 * time.Second,
	}
	m, _ := workspace.New(root, hooks)
	if _, _, err := m.EnsureCreated(context.Background(), "MT-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Remove(context.Background(), "MT-1"); err != nil {
		t.Fatalf("Remove: %v (best-effort hook failure must not propagate)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "MT-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace must be removed even when before_remove fails; stat err=%v", err)
	}
	if body, _ := os.ReadFile(marker); !strings.Contains(string(body), "gone") {
		t.Fatalf("before_remove must have fired; marker=%q", body)
	}
}

func TestRemoveOnMissingIsNoop(t *testing.T) {
	root := t.TempDir()
	m, _ := workspace.New(root, workspace.Hooks{})
	if err := m.Remove(context.Background(), "missing"); err != nil {
		t.Fatalf("Remove on missing: %v", err)
	}
}

func TestHookTimeoutTerminatesScript(t *testing.T) {
	root := t.TempDir()
	hooks := workspace.Hooks{
		AfterCreate: "sleep 5",
		Timeout:     150 * time.Millisecond,
	}
	m, _ := workspace.New(root, hooks)
	start := time.Now()
	_, _, err := m.EnsureCreated(context.Background(), "MT-1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("timeout took too long: %v", time.Since(start))
	}
}

func TestRunHookSetsEnvAndCwd(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "env.txt")
	hooks := workspace.Hooks{
		BeforeRun: "echo $SYMPHONY_ISSUE_IDENTIFIER:$SYMPHONY_HOOK:$(pwd) > " + marker,
		Timeout:   5 * time.Second,
	}
	m, _ := workspace.New(root, hooks)
	path, _, err := m.EnsureCreated(context.Background(), "MT-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RunHook(context.Background(), workspace.HookBeforeRun, path, "MT-1"); err != nil {
		t.Fatalf("RunHook: %v", err)
	}
	body, _ := os.ReadFile(marker)
	got := strings.TrimSpace(string(body))
	wantPrefix := "MT-1:before_run:"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("env: got %q want prefix %q", got, wantPrefix)
	}
	if !strings.HasSuffix(got, "/MT-1") {
		t.Fatalf("cwd: got %q want suffix /MT-1", got)
	}
}
