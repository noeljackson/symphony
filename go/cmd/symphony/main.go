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
	"syscall"
	"time"

	anthropicbackend "github.com/noeljackson/symphony/go/internal/backend/anthropicmessages"
	claudecodebackend "github.com/noeljackson/symphony/go/internal/backend/claudecode"
	codexbackend "github.com/noeljackson/symphony/go/internal/backend/codex"
	openaibackend "github.com/noeljackson/symphony/go/internal/backend/openaicompat"
	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/prompt"
	"github.com/noeljackson/symphony/go/internal/server"
	"github.com/noeljackson/symphony/go/internal/store"
	"github.com/noeljackson/symphony/go/internal/store/postgres"
	"github.com/noeljackson/symphony/go/internal/tracker"
	"github.com/noeljackson/symphony/go/internal/tracker/github"
	"github.com/noeljackson/symphony/go/internal/tracker/linear"
	"github.com/noeljackson/symphony/go/internal/workspace"
)

const databaseURLEnv = "SYMPHONY_DATABASE_URL"

func main() {
	// -1 sentinel = "not set"; 0 = "ephemeral bind"; positive = explicit
	// port. Uint can't represent the not-set case so we use signed int.
	port := flag.Int("port", -1, "HTTP server port (overrides server.port). 0 requests an ephemeral bind for tests.")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: symphony [--port N] [path-to-WORKFLOW.md]")
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
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

	ws, err := workspace.New(cfg.Workspace.Root, workspace.Hooks{
		AfterCreate:  derefStr(cfg.Hooks.AfterCreate),
		BeforeRun:    derefStr(cfg.Hooks.BeforeRun),
		AfterRun:     derefStr(cfg.Hooks.AfterRun),
		BeforeRemove: derefStr(cfg.Hooks.BeforeRemove),
		Timeout:      cfg.HookTimeout(),
	})
	if err != nil {
		return fmt.Errorf("workspace setup: %w", err)
	}
	pb, err := prompt.New(def.PromptTemplate)
	if err != nil {
		return fmt.Errorf("prompt template: %w", err)
	}

	tr, err := buildTracker(cfg)
	if err != nil {
		return fmt.Errorf("tracker setup: %w", err)
	}
	runner, err := buildBackend(cfg, ws, pb)
	if err != nil {
		return fmt.Errorf("backend setup: %w", err)
	}
	st, storeKind, err := buildStore(ctx, logger)
	if err != nil {
		return fmt.Errorf("state store setup: %w", err)
	}
	logger.Info("state store ready", slog.String("kind", storeKind))

	o, h := orchestrator.New(cfg, tr, runner, st, orchestrator.Options{
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

	srv, err := maybeStartHTTPServer(cfg, h, portOverride, logger)
	if err != nil {
		cancel()
		<-orchDone
		return err
	}

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
	srv, err := server.New(addr, h)
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

// Compile-time check: filepath.Clean is used implicitly via config.Workspace.Root.
var _ = filepath.Clean
