package doctor_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noeljackson/symphony/go/internal/doctor"
)

func writeWorkflow(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func runDoctor(t *testing.T, path string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code, err := doctor.Run(context.Background(), path, &out)
	if err != nil {
		t.Fatalf("doctor.Run: %v", err)
	}
	return out.String(), code
}

func TestDoctorReportsMissingWorkflow(t *testing.T) {
	out, code := runDoctor(t, "/no/such/workflow.md")
	if code != 1 {
		t.Fatalf("exit: got %d want 1", code)
	}
	if !strings.Contains(out, "workflow file loadable") || !strings.Contains(out, "✗") {
		t.Fatalf("output: %s", out)
	}
}

func TestDoctorReportsPreflightFailure(t *testing.T) {
	body := `---
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
	out, code := runDoctor(t, writeWorkflow(t, body))
	if code != 1 {
		t.Fatalf("exit: got %d want 1", code)
	}
	if !strings.Contains(out, "dispatch preflight") || !strings.Contains(out, "missing_tracker_api_key") {
		t.Fatalf("output: %s", out)
	}
}

func TestDoctorPassesAgainstReachableEndpoint(t *testing.T) {
	// Bind a TCP listener so the tracker reachability probe sees a live
	// host. Use the listener's actual addr.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	root := t.TempDir()
	body := `---
tracker:
  kind: linear
linear:
  endpoint: http://` + ln.Addr().String() + `
  api_key: test-key
  project_slug: demo
workspace:
  root: ` + root + `
agent:
  backend: openai_compat
openai_compat:
  endpoint: http://example.test
  api_key: test
  model: stub
  max_tokens: 64
---
body
`
	out, code := runDoctor(t, writeWorkflow(t, body))
	if code != 0 {
		t.Fatalf("exit: got %d want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "all checks passed") {
		t.Fatalf("missing summary line: %s", out)
	}
	for _, expected := range []string{
		"workflow file loadable",
		"dispatch preflight",
		"agent backend reachable",
		"workspace root writable",
		"hook scripts parse",
		"tracker endpoint reachable",
	} {
		if !strings.Contains(out, expected) {
			t.Fatalf("missing check %q in:\n%s", expected, out)
		}
	}
}

func TestDoctorFailsWhenStdioBinaryMissing(t *testing.T) {
	root := t.TempDir()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	body := `---
tracker:
  kind: linear
linear:
  endpoint: http://` + ln.Addr().String() + `
  api_key: k
  project_slug: demo
workspace:
  root: ` + root + `
agent:
  backend: claude_code
claude_code:
  command: definitely-not-on-PATH-claude-test --print
---
body
`
	out, code := runDoctor(t, writeWorkflow(t, body))
	if code != 1 {
		t.Fatalf("exit: got %d want 1", code)
	}
	if !strings.Contains(out, "claude_code command") || !strings.Contains(out, "not found on PATH") {
		t.Fatalf("output: %s", out)
	}
}

func TestDoctorFailsWhenHookSyntaxInvalid(t *testing.T) {
	root := t.TempDir()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	body := `---
tracker:
  kind: linear
linear:
  endpoint: http://` + ln.Addr().String() + `
  api_key: k
  project_slug: demo
workspace:
  root: ` + root + `
hooks:
  after_create: |
    if then
      echo broken
    fi
agent:
  backend: openai_compat
openai_compat:
  endpoint: http://example.test
  api_key: k
  model: m
  max_tokens: 64
---
body
`
	out, code := runDoctor(t, writeWorkflow(t, body))
	if code != 1 {
		t.Fatalf("exit: got %d want 1", code)
	}
	if !strings.Contains(out, "hook scripts parse") {
		t.Fatalf("output: %s", out)
	}
}
