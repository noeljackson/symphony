// Package doctor implements the `symphony doctor` first-run preflight per
// SPEC v3 §18.2.
//
// Each [Check] is a single named pass/fail line in the output. The
// orchestrator's dispatch preflight (§6.3) is wrapped as a single check;
// environment-level checks (binary on PATH, workspace writable, hook
// scripts parse, tracker auth reachable) extend it.
package doctor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/noeljackson/symphony/go/internal/config"
)

// Result is one row of the report.
type Result struct {
	Name string
	Pass bool
	// Detail is a one-line explanation. For passes it's a short summary
	// (e.g. "linear endpoint reachable"); for failures it surfaces the
	// underlying error verbatim.
	Detail string
}

// Run executes every preflight check against the workflow at path. The
// report is written to out (one line per check); the returned int is the
// process exit code (0 on full green, 1 on any failure).
func Run(ctx context.Context, path string, out io.Writer) (int, error) {
	results := make([]Result, 0, 8)

	def, err := config.LoadWorkflow(path)
	if err != nil {
		results = append(results, Result{
			Name:   "workflow file loadable",
			Pass:   false,
			Detail: err.Error(),
		})
		return finishReport(out, results), nil
	}
	results = append(results, Result{
		Name:   "workflow file loadable",
		Pass:   true,
		Detail: filepath.Clean(def.Path),
	})

	cfg := def.Config
	results = append(results, dispatchPreflight(cfg))
	results = append(results, agentBinaryCheck(cfg))
	results = append(results, workspaceWritableCheck(cfg))
	results = append(results, hookScriptsParseCheck(cfg))
	results = append(results, trackerAuthCheck(ctx, cfg))

	return finishReport(out, results), nil
}

func finishReport(out io.Writer, results []Result) int {
	failed := 0
	for _, r := range results {
		mark := "✓"
		if !r.Pass {
			mark = "✗"
			failed++
		}
		fmt.Fprintf(out, "%s %s — %s\n", mark, r.Name, r.Detail)
	}
	if failed == 0 {
		fmt.Fprintln(out, "all checks passed")
	} else {
		fmt.Fprintf(out, "%d check(s) failed\n", failed)
	}
	if failed == 0 {
		return 0
	}
	return 1
}

func dispatchPreflight(cfg *config.ServiceConfig) Result {
	if err := cfg.ValidateForDispatch(); err != nil {
		return Result{Name: "dispatch preflight", Pass: false, Detail: err.Error()}
	}
	return Result{
		Name: "dispatch preflight",
		Pass: true,
		Detail: fmt.Sprintf("tracker=%s backend=%s",
			cfg.Tracker.Kind, cfg.Agent.Backend),
	}
}

// agentBinaryCheck verifies that stdio backends have their CLI on PATH.
// HTTP backends always pass — there's no local binary to check.
func agentBinaryCheck(cfg *config.ServiceConfig) Result {
	switch cfg.Agent.Backend {
	case config.BackendCodex:
		return resolveBinaryFromCommand("codex command", cfg.Codex.Command)
	case config.BackendClaudeCode:
		return resolveBinaryFromCommand("claude_code command", cfg.ClaudeCode.Command)
	case config.BackendOpenAICompat:
		return Result{Name: "agent backend reachable", Pass: true,
			Detail: "openai_compat: HTTP endpoint (no local binary)"}
	case config.BackendAnthropicMessages:
		return Result{Name: "agent backend reachable", Pass: true,
			Detail: "anthropic_messages: HTTP endpoint (no local binary)"}
	}
	return Result{Name: "agent backend reachable", Pass: false,
		Detail: fmt.Sprintf("unsupported backend %s", cfg.Agent.Backend)}
}

func resolveBinaryFromCommand(label, cmd string) Result {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return Result{Name: label, Pass: false, Detail: "command is empty"}
	}
	bin := parts[0]
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return Result{Name: label, Pass: false,
			Detail: fmt.Sprintf("%s not found on PATH (%v)", bin, err)}
	}
	return Result{Name: label, Pass: true, Detail: resolved}
}

func workspaceWritableCheck(cfg *config.ServiceConfig) Result {
	root := cfg.Workspace.Root
	if root == "" {
		return Result{Name: "workspace root writable", Pass: false, Detail: "workspace.root is empty"}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Result{Name: "workspace root writable", Pass: false,
			Detail: fmt.Sprintf("mkdir %s: %v", root, err)}
	}
	probe, err := os.CreateTemp(root, ".symphony-doctor-")
	if err != nil {
		return Result{Name: "workspace root writable", Pass: false,
			Detail: fmt.Sprintf("write probe in %s: %v", root, err)}
	}
	probePath := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probePath)
	return Result{Name: "workspace root writable", Pass: true, Detail: root}
}

func hookScriptsParseCheck(cfg *config.ServiceConfig) Result {
	scripts := map[string]*string{
		"after_create":  cfg.Hooks.AfterCreate,
		"before_run":    cfg.Hooks.BeforeRun,
		"after_run":     cfg.Hooks.AfterRun,
		"before_remove": cfg.Hooks.BeforeRemove,
	}
	checked := 0
	for name, script := range scripts {
		if script == nil || strings.TrimSpace(*script) == "" {
			continue
		}
		checked++
		cmd := exec.Command("bash", "-n", "-c", *script)
		if out, err := cmd.CombinedOutput(); err != nil {
			return Result{Name: "hook scripts parse", Pass: false,
				Detail: fmt.Sprintf("%s: %v (%s)", name, err, strings.TrimSpace(string(out)))}
		}
	}
	if checked == 0 {
		return Result{Name: "hook scripts parse", Pass: true, Detail: "no hooks configured"}
	}
	return Result{Name: "hook scripts parse", Pass: true,
		Detail: fmt.Sprintf("%d script(s) accepted by `bash -n`", checked)}
}

// trackerAuthCheck pokes the configured tracker endpoint. We don't
// authenticate or read data — just confirm the endpoint resolves and
// accepts a TCP connection — so this stays cheap and safe to run from
// `symphony doctor` without burning rate limits.
func trackerAuthCheck(ctx context.Context, cfg *config.ServiceConfig) Result {
	endpoint := ""
	switch cfg.Tracker.Kind {
	case config.TrackerLinear:
		endpoint = cfg.Linear.Endpoint
	case config.TrackerGitHub:
		endpoint = cfg.GitHub.Endpoint
	}
	if endpoint == "" {
		return Result{Name: "tracker endpoint reachable", Pass: false,
			Detail: "no endpoint configured"}
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return Result{Name: "tracker endpoint reachable", Pass: false,
			Detail: fmt.Sprintf("parse %q: %v", endpoint, err)}
	}
	host := parsed.Host
	if !strings.Contains(host, ":") {
		if parsed.Scheme == "https" {
			host = host + ":443"
		} else {
			host = host + ":80"
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(dctx, "tcp", host)
	if err != nil {
		return Result{Name: "tracker endpoint reachable", Pass: false,
			Detail: fmt.Sprintf("dial %s: %v", host, err)}
	}
	_ = conn.Close()
	return Result{Name: "tracker endpoint reachable", Pass: true, Detail: endpoint}
}
