// Command symphony runs the SPEC v3 orchestrator end-to-end.
//
// Layout:
//  1. Load WORKFLOW.md → ServiceConfig + dispatch preflight.
//  2. Build the workspace manager + prompt renderer.
//  3. Build the tracker (linear / github) per cfg.Tracker.Kind.
//  4. Build the agent backend (codex / claude_code / openai_compat /
//     anthropic_messages) per cfg.Agent.Backend.
//  5. Build the state store (PostgreSQL when DATABASE_URL is set;
//     in-memory otherwise — see SPEC §4.1.9 for the contract).
//  6. Construct the orchestrator and (optionally) the §13.7 HTTP
//     server.
//  7. Drive ticks until SIGINT / SIGTERM, then shut down cleanly.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	anthropicbackend "github.com/noeljackson/symphony/go/internal/backend/anthropicmessages"
	claudecodebackend "github.com/noeljackson/symphony/go/internal/backend/claudecode"
	codexbackend "github.com/noeljackson/symphony/go/internal/backend/codex"
	openaibackend "github.com/noeljackson/symphony/go/internal/backend/openaicompat"
	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/dashboard"
	"github.com/noeljackson/symphony/go/internal/doctor"
	"github.com/noeljackson/symphony/go/internal/logsclient"
	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/prompt"
	"github.com/noeljackson/symphony/go/internal/server"
	"github.com/noeljackson/symphony/go/internal/store"
	"github.com/noeljackson/symphony/go/internal/store/postgres"
	"github.com/noeljackson/symphony/go/internal/tracker"
	"github.com/noeljackson/symphony/go/internal/tracker/github"
	"github.com/noeljackson/symphony/go/internal/tracker/linear"
	"github.com/noeljackson/symphony/go/internal/watcher"
	"github.com/noeljackson/symphony/go/internal/workspace"
)

const databaseURLEnv = "SYMPHONY_DATABASE_URL"

func main() {
	// Subcommand dispatch: `doctor` and `logs` are dispatched first so their
	// flags don't collide with the daemon's `--port`. Anything else falls
	// through to the daemon (the foundation behavior).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "doctor":
			os.Exit(runDoctor(os.Args[2:]))
		case "logs":
			os.Exit(runLogs(os.Args[2:]))
		case "-h", "--help", "help":
			printUsage()
			return
		}
	}
	runDaemon(os.Args[1:])
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: symphony [<subcommand>] [args...]")
	fmt.Fprintln(os.Stderr, "subcommands:")
	fmt.Fprintln(os.Stderr, "  (default)            run the orchestrator daemon")
	fmt.Fprintln(os.Stderr, "  doctor [path]        run preflight + environment checks")
	fmt.Fprintln(os.Stderr, "  logs <id> --url URL  tail per-issue agent activity")
}

func runDaemon(rawArgs []string) {
	fs := flag.NewFlagSet("symphony", flag.ExitOnError)
	port := fs.Int("port", -1, "HTTP server port (overrides server.port). 0 requests an ephemeral bind for tests.")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: symphony [--port N] [path-to-WORKFLOW.md]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(rawArgs)

	args := fs.Args()
	path := "./WORKFLOW.md"
	if len(args) > 0 {
		path = args[0]
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if err := run(context.Background(), path, *port, logger); err != nil {
		fmt.Fprintf(os.Stderr, "symphony: %v\n", err)
		os.Exit(1)
	}
}

func runDoctor(rawArgs []string) int {
	fs := flag.NewFlagSet("symphony doctor", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: symphony doctor [path-to-WORKFLOW.md]")
	}
	_ = fs.Parse(rawArgs)
	args := fs.Args()
	path := "./WORKFLOW.md"
	if len(args) > 0 {
		path = args[0]
	}
	code, err := doctor.Run(context.Background(), path, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "symphony doctor: %v\n", err)
		return 2
	}
	return code
}

func runLogs(rawArgs []string) int {
	fs := flag.NewFlagSet("symphony logs", flag.ExitOnError)
	url := fs.String("url", "", "HTTP URL of the running orchestrator (required)")
	noFollow := fs.Bool("no-follow", false, "print backfill from /api/v1/<id> and exit")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: symphony logs <issue-identifier> --url URL [--no-follow]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(rawArgs)
	args := fs.Args()
	if len(args) < 1 {
		fs.Usage()
		return 2
	}
	if strings.TrimSpace(*url) == "" {
		fmt.Fprintln(os.Stderr, "symphony logs: --url is required")
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	code, err := logsclient.Run(ctx, logsclient.Args{
		Identifier: args[0],
		URL:        *url,
		Follow:     !*noFollow,
	}, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "symphony logs: %v\n", err)
		return 2
	}
	return code
}

func run(ctx context.Context, workflowPath string, portOverride int, logger *slog.Logger) error {
	def, err := config.LoadWorkflow(workflowPath)
	if err != nil {
		return fmt.Errorf("load workflow: %w", err)
	}
	cfg := def.Config
	if err := cfg.ValidateForDispatch(); err != nil {
		return fmt.Errorf("dispatch preflight failed: %w", err)
	}
	logger.Info("workflow loaded",
		slog.String("path", def.Path),
		slog.String("tracker", string(cfg.Tracker.Kind)),
		slog.String("backend", string(cfg.Agent.Backend)))

	cs, err := buildComponents(def, nil)
	if err != nil {
		return err
	}
	st, storeKind, err := buildStore(ctx, logger)
	if err != nil {
		return fmt.Errorf("state store setup: %w", err)
	}
	logger.Info("state store ready", slog.String("kind", storeKind))

	o, h := orchestrator.New(cs.cfg, cs.tracker, cs.runner, st, orchestrator.Options{
		Logger:       logger,
		AutoSchedule: true,
	})

	runCtx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	orchDone := make(chan struct{})
	go func() {
		o.Run(runCtx)
		close(orchDone)
	}()

	srv, err := maybeStartHTTPServer(cs.cfg, h, portOverride, logger)
	if err != nil {
		cancel()
		<-orchDone
		return err
	}

	// SPEC §6.2: hot-reload on WORKFLOW.md change. Watcher failures are
	// non-fatal — the orchestrator keeps running with the boot-time cfg.
	startWorkflowWatcher(runCtx, def.Path, h, cs, logger)

	// First tick fires immediately; the auto-scheduler keeps it going.
	h.Tick(runCtx)

	<-runCtx.Done()
	logger.Info("shutting down")

	if srv != nil {
		shutdownCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(shutdownCtx)
		sCancel()
	}
	h.Shutdown(context.Background())
	<-orchDone
	return nil
}

func buildTracker(cfg *config.ServiceConfig) (tracker.Tracker, error) {
	switch cfg.Tracker.Kind {
	case config.TrackerLinear:
		return linear.New(linear.Config{
			Endpoint:       cfg.Linear.Endpoint,
			APIKey:         cfg.Linear.APIKey,
			ProjectSlug:    cfg.Linear.ProjectSlug,
			ActiveStates:   cfg.Tracker.ActiveStates,
			TerminalStates: cfg.Tracker.TerminalStates,
		})
	case config.TrackerGitHub:
		return github.New(github.Config{
			Endpoint:         cfg.GitHub.Endpoint,
			Owner:            cfg.GitHub.Owner,
			Repo:             cfg.GitHub.Repo,
			APIToken:         cfg.GitHub.APIToken,
			LabelPriorityMap: cfg.GitHub.LabelPriorityMap,
			Assignee:         cfg.GitHub.Assignee,
			ActiveStates:     cfg.Tracker.ActiveStates,
			TerminalStates:   cfg.Tracker.TerminalStates,
		})
	}
	return nil, fmt.Errorf("unsupported tracker kind: %s", cfg.Tracker.Kind)
}

func buildBackend(
	cfg *config.ServiceConfig,
	ws *workspace.Manager,
	pb *prompt.Builder,
) (orchestrator.WorkerRunner, error) {
	switch cfg.Agent.Backend {
	case config.BackendCodex:
		return codexbackend.New(codexbackend.Config{
			Codex:     cfg.Codex,
			Agent:     cfg.Agent,
			Workspace: ws,
			Prompt:    pb,
		})
	case config.BackendClaudeCode:
		return claudecodebackend.New(claudecodebackend.Config{
			ClaudeCode: cfg.ClaudeCode,
			Agent:      cfg.Agent,
			Workspace:  ws,
			Prompt:     pb,
		})
	case config.BackendOpenAICompat:
		return openaibackend.New(openaibackend.Config{
			OpenAICompat: cfg.OpenAICompat,
			Agent:        cfg.Agent,
			Workspace:    ws,
			Prompt:       pb,
		})
	case config.BackendAnthropicMessages:
		return anthropicbackend.New(anthropicbackend.Config{
			AnthropicMessages: cfg.AnthropicMessages,
			Agent:             cfg.Agent,
			Workspace:         ws,
			Prompt:            pb,
		})
	}
	return nil, fmt.Errorf("unsupported agent backend: %s", cfg.Agent.Backend)
}

// buildStore returns the configured state store: Postgres when
// SYMPHONY_DATABASE_URL is set, in-memory otherwise. The kind string is
// surfaced for startup logging.
func buildStore(ctx context.Context, logger *slog.Logger) (store.Store, string, error) {
	url := os.Getenv(databaseURLEnv)
	if url == "" {
		logger.Warn("SYMPHONY_DATABASE_URL not set; using in-memory state store (state will not survive restart)")
		return store.NewMemoryStore(), "memory", nil
	}
	pg, err := postgres.Open(url)
	if err != nil {
		return nil, "", fmt.Errorf("open postgres: %w", err)
	}
	if err := pg.Migrate(ctx); err != nil {
		_ = pg.Close()
		return nil, "", fmt.Errorf("migrate postgres: %w", err)
	}
	return pg, "postgres", nil
}

func maybeStartHTTPServer(
	cfg *config.ServiceConfig,
	h *orchestrator.Handle,
	portOverride int,
	logger *slog.Logger,
) (*server.HTTPServer, error) {
	port := uint16(0)
	enabled := false
	switch {
	case portOverride >= 0:
		// Explicit --port set, including --port 0 for an ephemeral bind.
		if portOverride > 65535 {
			return nil, fmt.Errorf("--port out of range: %d", portOverride)
		}
		port = uint16(portOverride)
		enabled = true
	case cfg.Server.Port != nil:
		port = *cfg.Server.Port
		enabled = true
	}
	if !enabled {
		logger.Info("HTTP server disabled (no --port and no server.port in workflow)")
		return nil, nil
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv, err := server.New(addr, h, server.Option(dashboard.Mount))
	if err != nil {
		return nil, fmt.Errorf("start http server: %w", err)
	}
	go func() {
		if err := srv.Start(); err != nil {
			logger.Error("http server stopped", slog.String("err", err.Error()))
		}
	}()
	logger.Info("http server listening", slog.String("addr", srv.Addr()))
	return srv, nil
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// componentSet bundles the orchestrator's runtime dependencies that
// closed-at-boot-time over a particular ServiceConfig + prompt
// template. On a §6.2 hot-reload, [buildComponents] reuses any
// dependency whose backing config didn't change and rebuilds the rest.
type componentSet struct {
	def     *config.WorkflowDefinition
	cfg     *config.ServiceConfig
	ws      *workspace.Manager
	pb      *prompt.Builder
	tracker tracker.Tracker
	runner  orchestrator.WorkerRunner
}

// buildComponents constructs the orchestrator's runtime dependencies
// for def. When prev is non-nil, the function reuses each dependency
// whose backing config slice didn't change; this keeps in-flight
// workspaces / tracker connections / backend HTTP clients alive across
// reloads so the actor can swap them atomically with no stale state.
func buildComponents(def *config.WorkflowDefinition, prev *componentSet) (*componentSet, error) {
	cfg := def.Config
	cs := &componentSet{def: def, cfg: cfg}

	// Workspace: rebuild on root or hooks change.
	if prev != nil && prev.cfg.Workspace.Root == cfg.Workspace.Root && hooksEqual(prev.cfg.Hooks, cfg.Hooks) {
		cs.ws = prev.ws
	} else {
		ws, err := workspace.New(cfg.Workspace.Root, workspace.Hooks{
			AfterCreate:  derefStr(cfg.Hooks.AfterCreate),
			BeforeRun:    derefStr(cfg.Hooks.BeforeRun),
			AfterRun:     derefStr(cfg.Hooks.AfterRun),
			BeforeRemove: derefStr(cfg.Hooks.BeforeRemove),
			Timeout:      cfg.HookTimeout(),
		})
		if err != nil {
			return nil, fmt.Errorf("workspace setup: %w", err)
		}
		cs.ws = ws
	}

	// Prompt template: rebuild only if the body changed.
	if prev != nil && prev.def.PromptTemplate == def.PromptTemplate {
		cs.pb = prev.pb
	} else {
		pb, err := prompt.New(def.PromptTemplate)
		if err != nil {
			return nil, fmt.Errorf("prompt template: %w", err)
		}
		cs.pb = pb
	}

	// Tracker: rebuild on kind / auth / endpoint change.
	if prev != nil && trackerSettingsEqual(prev.cfg, cfg) {
		cs.tracker = prev.tracker
	} else {
		tr, err := buildTracker(cfg)
		if err != nil {
			return nil, fmt.Errorf("tracker setup: %w", err)
		}
		cs.tracker = tr
	}

	// Backend: rebuild on backend selection / per-backend config / shared
	// dependency (workspace, prompt) change.
	backendDepsChanged := prev == nil ||
		!backendSettingsEqual(prev.cfg, cfg) ||
		cs.ws != prev.ws ||
		cs.pb != prev.pb
	if !backendDepsChanged {
		cs.runner = prev.runner
	} else {
		runner, err := buildBackend(cfg, cs.ws, cs.pb)
		if err != nil {
			return nil, fmt.Errorf("backend setup: %w", err)
		}
		cs.runner = runner
	}

	return cs, nil
}

func hooksEqual(a, b config.HooksConfig) bool {
	return strPtrEq(a.AfterCreate, b.AfterCreate) &&
		strPtrEq(a.BeforeRun, b.BeforeRun) &&
		strPtrEq(a.AfterRun, b.AfterRun) &&
		strPtrEq(a.BeforeRemove, b.BeforeRemove) &&
		a.TimeoutMS == b.TimeoutMS
}

func strPtrEq(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func trackerSettingsEqual(a, b *config.ServiceConfig) bool {
	if a.Tracker.Kind != b.Tracker.Kind {
		return false
	}
	if !sliceStringEq(a.Tracker.ActiveStates, b.Tracker.ActiveStates) ||
		!sliceStringEq(a.Tracker.TerminalStates, b.Tracker.TerminalStates) {
		return false
	}
	switch a.Tracker.Kind {
	case config.TrackerLinear:
		return a.Linear == b.Linear
	case config.TrackerGitHub:
		return a.GitHub.Endpoint == b.GitHub.Endpoint &&
			a.GitHub.Owner == b.GitHub.Owner &&
			a.GitHub.Repo == b.GitHub.Repo &&
			a.GitHub.APIToken == b.GitHub.APIToken &&
			a.GitHub.AppID == b.GitHub.AppID &&
			a.GitHub.AppInstallationID == b.GitHub.AppInstallationID &&
			a.GitHub.PrivateKey == b.GitHub.PrivateKey &&
			a.GitHub.Assignee == b.GitHub.Assignee &&
			mapStringIntEqual(a.GitHub.LabelPriorityMap, b.GitHub.LabelPriorityMap)
	}
	return true
}

func backendSettingsEqual(a, b *config.ServiceConfig) bool {
	if a.Agent.Backend != b.Agent.Backend ||
		a.Agent.MaxTurns != b.Agent.MaxTurns {
		return false
	}
	switch a.Agent.Backend {
	case config.BackendCodex:
		return a.Codex.Command == b.Codex.Command &&
			a.Codex.TurnTimeoutMS == b.Codex.TurnTimeoutMS &&
			a.Codex.ReadTimeoutMS == b.Codex.ReadTimeoutMS &&
			a.Codex.StallTimeoutMS == b.Codex.StallTimeoutMS
	case config.BackendClaudeCode:
		return a.ClaudeCode.Command == b.ClaudeCode.Command &&
			a.ClaudeCode.PermissionMode == b.ClaudeCode.PermissionMode &&
			a.ClaudeCode.Model == b.ClaudeCode.Model &&
			a.ClaudeCode.TurnTimeoutMS == b.ClaudeCode.TurnTimeoutMS &&
			a.ClaudeCode.ReadTimeoutMS == b.ClaudeCode.ReadTimeoutMS &&
			a.ClaudeCode.StallTimeoutMS == b.ClaudeCode.StallTimeoutMS &&
			sliceStringEq(a.ClaudeCode.AllowedTools, b.ClaudeCode.AllowedTools) &&
			sliceStringEq(a.ClaudeCode.DisallowedTools, b.ClaudeCode.DisallowedTools)
	case config.BackendOpenAICompat:
		return a.OpenAICompat == b.OpenAICompat
	case config.BackendAnthropicMessages:
		return a.AnthropicMessages == b.AnthropicMessages
	}
	return true
}

func sliceStringEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mapStringIntEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// startWorkflowWatcher subscribes to filesystem changes on path and
// forwards each successful reload to the orchestrator (SPEC §6.2). On
// each event the previous componentSet is diffed against the new one,
// dependencies whose backing config changed are rebuilt, and the result
// is shipped to the actor via Handle.Reload's ReloadComponents struct.
// Validation/build errors are logged at warn level; the orchestrator
// keeps running with the last-known-good config per SPEC §6.2.
func startWorkflowWatcher(ctx context.Context, path string, h *orchestrator.Handle, initial *componentSet, logger *slog.Logger) {
	events, err := watcher.Watch(ctx, path)
	if err != nil {
		logger.Warn("workflow watcher disabled", slog.String("path", path), slog.String("err", err.Error()))
		return
	}
	go func() {
		logger.Info("workflow watcher started", slog.String("path", path))
		current := initial
		for ev := range events {
			if ev.Err != nil {
				logger.Warn("workflow reload failed; keeping last-known-good config",
					slog.String("err", ev.Err.Error()))
				continue
			}
			next, err := buildComponents(ev.Definition, current)
			if err != nil {
				logger.Warn("workflow reload rebuild failed; keeping last-known-good components",
					slog.String("err", err.Error()))
				continue
			}
			components := orchestrator.ReloadComponents{}
			if next.tracker != current.tracker {
				components.Tracker = next.tracker
			}
			if next.runner != current.runner {
				components.Runner = next.runner
			}
			outcome := h.Reload(ctx, ev.Definition, components)
			if outcome.Err != nil {
				logger.Warn("workflow reload rejected by orchestrator",
					slog.String("err", outcome.Err.Error()))
				continue
			}
			logger.Info("workflow reload applied",
				slog.Any("hot_swapped", outcome.Affected),
				slog.Bool("swapped_tracker", outcome.SwappedTracker),
				slog.Bool("swapped_runner", outcome.SwappedRunner),
				slog.Bool("restart_required", outcome.Restart))
			if outcome.Restart {
				logger.Warn("some workflow keys still require a process restart to take full effect")
			}
			current = next
		}
	}()
}

// Compile-time check: filepath.Clean is used implicitly via config.Workspace.Root.
var _ = filepath.Clean
