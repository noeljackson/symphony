package main_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildBinary compiles the symphony binary into a temp file once per test
// run so each test can re-exec it without paying the build cost.
var buildOnce sync.Once
var binaryPath string
var buildErr error

func ensureBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "symphony-binary-*")
		if err != nil {
			buildErr = err
			return
		}
		binaryPath = filepath.Join(dir, "symphony")
		cmd := exec.Command("go", "build", "-o", binaryPath, ".")
		cmd.Stderr = os.Stderr
		buildErr = cmd.Run()
	})
	if buildErr != nil {
		t.Fatalf("build symphony binary: %v", buildErr)
	}
	return binaryPath
}

func writeWorkflow(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func TestSymphonyExitsCleanlyOnMissingWorkflow(t *testing.T) {
	bin := ensureBinary(t)
	cmd := exec.Command(bin, "/no/such/workflow.md")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit on missing workflow")
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("expected exit 1, got %v", err)
	}
	if !strings.Contains(string(out), "load workflow") {
		t.Fatalf("output: %s", out)
	}
}

func TestSymphonyExitsOnPreflightFailure(t *testing.T) {
	bin := ensureBinary(t)
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
	path := writeWorkflow(t, body)
	cmd := exec.Command(bin, path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit on missing api_key")
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("expected exit 1, got %v", err)
	}
	if !strings.Contains(string(out), "preflight failed") {
		t.Fatalf("output: %s", out)
	}
}

// TestSymphonyEndToEnd boots the binary with fake Linear + fake
// openai-compatible servers, asks it to bind an HTTP server on an
// ephemeral port, polls /api/v1/state to confirm the runtime is alive,
// then sends SIGTERM and waits for clean exit.
func TestSymphonyEndToEnd(t *testing.T) {
	bin := ensureBinary(t)

	// Fake Linear server — returns one Todo issue.
	linearSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data":{"project":{"issues":{"nodes":[
				{"id":"a","identifier":"MT-1","title":"alpha","priority":1,"state":{"name":"Todo"}}
			]}}}
		}`))
	}))
	defer linearSrv.Close()

	// Fake openai-compatible chat completions endpoint — returns a stub
	// response so the worker exits Success.
	openaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"x",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
		}`))
	}))
	defer openaiSrv.Close()

	wsRoot := t.TempDir()
	body := `---
tracker:
  kind: linear
  active_states: [Todo]
  terminal_states: [Done]
linear:
  endpoint: ` + linearSrv.URL + `
  api_key: test-linear-key
  project_slug: demo
polling:
  interval_ms: 200
workspace:
  root: ` + wsRoot + `
agent:
  backend: openai_compat
  max_concurrent_agents: 1
openai_compat:
  endpoint: ` + openaiSrv.URL + `
  api_key: test-openai-key
  model: stub-model
  max_tokens: 64
server:
  port: 0
---
Issue {{ issue.identifier }}: {{ issue.title }}
`
	path := writeWorkflow(t, body)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, path)
	stdoutBuf := &lineBuffer{}
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stdoutBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()

	addr := waitForHTTPAddr(t, stdoutBuf, 5*time.Second)

	// Poll /api/v1/state until 200 OK.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/api/v1/state")
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var got map[string]any
				_ = json.NewDecoder(resp.Body).Decode(&got)
				if _, ok := got["counts"]; !ok {
					t.Fatalf("/state body missing counts: %v", got)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("symphony /state never returned 200; output:\n%s", stdoutBuf.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Clean shutdown.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		// SIGINT-driven exits are typically code 0; some shells/wrappers
		// surface 130. Anything else is a real failure.
		if exit, ok := waitErr.(*exec.ExitError); ok && exit.ExitCode() != 130 && exit.ExitCode() != 0 {
			t.Fatalf("symphony exited with %d, output:\n%s", exit.ExitCode(), stdoutBuf.String())
		}
	}
}

// waitForHTTPAddr scans symphony's stderr for the "http server listening"
// log line and extracts the addr value.
func waitForHTTPAddr(t *testing.T, buf *lineBuffer, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		line := buf.findLine("http server listening")
		if line != "" {
			i := strings.Index(line, "addr=")
			if i >= 0 {
				rest := line[i+len("addr="):]
				rest = strings.TrimSpace(rest)
				rest = strings.Trim(rest, `"`)
				return rest
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("symphony never logged 'http server listening'; output:\n%s", buf.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// lineBuffer is a goroutine-safe io.Writer that lets the test scan
// already-emitted lines for log markers.
type lineBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *lineBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf = append(b.buf, p...)
	b.mu.Unlock()
	return len(p), nil
}

func (b *lineBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func (b *lineBuffer) findLine(needle string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, line := range strings.Split(string(b.buf), "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
