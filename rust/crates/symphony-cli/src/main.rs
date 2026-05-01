//! `symphony` binary. SPEC §17.7 + §18.2 (`symphony doctor`).

mod doctor;

use std::net::SocketAddr;
use std::path::PathBuf;
use std::process::ExitCode;
use std::sync::Arc;

use clap::{Parser, Subcommand};
use symphony_codex::tools::ToolExecutor;
use symphony_core::config::TrackerKind;
use symphony_core::prompt::PromptBuilder;
use symphony_core::watcher::{ReloadEvent, WorkflowWatcher};
use symphony_core::workflow::WorkflowLoader;
use symphony_core::ServiceConfig;
use symphony_orchestrator::{Orchestrator, RealWorker, WorkspaceCleaner, WorkspaceManagerCleaner};
use symphony_tracker::linear::{GraphqlTransport, LinearClient, LinearConfig, ReqwestTransport};
use symphony_tracker::linear_tool::LinearGraphqlTool;
use symphony_tracker::Tracker;
use symphony_workspace::WorkspaceManager;

#[derive(Parser, Debug)]
#[command(
    name = "symphony",
    version,
    about = "Symphony coding-agent orchestrator"
)]
struct Cli {
    /// Path to `WORKFLOW.md`. Defaults to `./WORKFLOW.md` in the current
    /// working directory. Used by both the daemon and `symphony doctor`.
    workflow_path: Option<PathBuf>,

    /// Optional HTTP server port. Overrides `server.port` in the workflow.
    /// `0` requests an ephemeral port. Daemon mode only.
    #[arg(long)]
    port: Option<u16>,

    #[command(subcommand)]
    command: Option<Command>,
}

#[derive(Subcommand, Debug)]
enum Command {
    /// Run preflight + environment checks against the workflow and print a
    /// pass/fail checklist. Exit `0` on full green, `1` on any failure.
    Doctor {
        /// Path to `WORKFLOW.md`. Defaults to `./WORKFLOW.md`.
        workflow_path: Option<PathBuf>,
    },
}

fn main() -> ExitCode {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();

    let cli = Cli::parse();

    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            eprintln!("symphony: failed to start tokio runtime: {e}");
            return ExitCode::FAILURE;
        }
    };

    match cli.command {
        Some(Command::Doctor { workflow_path }) => {
            let path = workflow_path
                .or(cli.workflow_path)
                .unwrap_or_else(|| PathBuf::from("./WORKFLOW.md"));
            runtime.block_on(async move { run_doctor(&path).await })
        }
        None => {
            let path = cli
                .workflow_path
                .unwrap_or_else(|| PathBuf::from("./WORKFLOW.md"));
            if !path.exists() {
                eprintln!("symphony: workflow file not found: {}", path.display());
                return ExitCode::from(2);
            }
            runtime.block_on(async move { run(path, cli.port).await })
        }
    }
}

async fn run_doctor(path: &std::path::Path) -> ExitCode {
    let mut stdout = std::io::stdout();
    match doctor::run(path, &mut stdout).await {
        Ok(0) => ExitCode::SUCCESS,
        Ok(_) => ExitCode::FAILURE,
        Err(e) => {
            eprintln!("symphony doctor: io error: {e}");
            ExitCode::FAILURE
        }
    }
}

async fn run(path: PathBuf, port_override: Option<u16>) -> ExitCode {
    let definition = match WorkflowLoader::load(&path) {
        Ok(d) => d,
        Err(e) => {
            eprintln!("symphony: failed to load workflow: {e}");
            return ExitCode::FAILURE;
        }
    };
    let cfg = match ServiceConfig::from_workflow(&definition) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("symphony: invalid workflow config: {e}");
            return ExitCode::FAILURE;
        }
    };
    if let Err(e) = cfg.validate_for_dispatch() {
        eprintln!("symphony: dispatch preflight failed: {e}");
        return ExitCode::FAILURE;
    }
    let cfg = Arc::new(cfg);

    let (tracker, graphql_transport): (Arc<dyn Tracker>, Arc<dyn GraphqlTransport>) =
        match cfg.tracker.kind {
            TrackerKind::Linear => {
                let transport: Arc<dyn GraphqlTransport> = Arc::new(ReqwestTransport::new(
                    cfg.tracker.endpoint.clone(),
                    cfg.tracker.api_key.clone().unwrap_or_default(),
                ));
                let client = match LinearClient::new(LinearConfig {
                    endpoint: cfg.tracker.endpoint.clone(),
                    api_key: cfg.tracker.api_key.clone().unwrap_or_default(),
                    project_slug: cfg.tracker.project_slug.clone().unwrap_or_default(),
                    active_states: cfg.tracker.active_states.clone(),
                    terminal_states: cfg.tracker.terminal_states.clone(),
                }) {
                    Ok(c) => c,
                    Err(e) => {
                        eprintln!("symphony: failed to build Linear client: {e}");
                        return ExitCode::FAILURE;
                    }
                };
                (Arc::new(client), transport)
            }
            TrackerKind::Other(ref k) => {
                eprintln!("symphony: unsupported tracker kind: {k}");
                return ExitCode::FAILURE;
            }
        };

    let workspace_mgr = Arc::new(WorkspaceManager::new(
        cfg.workspace.root.clone(),
        cfg.hooks.timeout_ms,
        cfg.hooks.after_create.clone(),
        cfg.hooks.before_remove.clone(),
    ));

    let prompt_builder = Arc::new(PromptBuilder::new(&definition.prompt_template));

    // Best-effort: clean up workspaces for issues already in terminal states
    // before scheduling the first tick (SPEC §8.6).
    if let Ok(stale) = tracker
        .fetch_issues_by_states(&cfg.tracker.terminal_states)
        .await
    {
        for issue in stale {
            if let Err(e) = workspace_mgr.remove(&issue.identifier).await {
                tracing::warn!(identifier = %issue.identifier, error = %e, "terminal cleanup failed");
            }
        }
    } else {
        tracing::warn!("terminal cleanup fetch failed; continuing startup");
    }

    let tools: Arc<dyn ToolExecutor> = Arc::new(LinearGraphqlTool::new(graphql_transport));
    let runner = Arc::new(
        RealWorker::new(
            cfg.clone(),
            workspace_mgr.clone(),
            tracker.clone(),
            prompt_builder,
        )
        .with_tools(tools),
    );

    let cleaner: Arc<dyn WorkspaceCleaner> = Arc::new(WorkspaceManagerCleaner {
        manager: workspace_mgr.clone(),
    });
    let (actor, handle) = Orchestrator::new(cfg.clone(), tracker, runner);
    let actor = actor.with_auto_schedule(true).with_cleaner(cleaner);
    let actor_join = tokio::spawn(async move {
        let _ = actor.run().await;
    });

    // Start the workflow watcher so config edits hot-reload.
    let watcher_handle = handle.clone();
    let watcher_path = definition.path.clone();
    let _watch_task = tokio::spawn(async move {
        let mut watcher = match WorkflowWatcher::start(&watcher_path) {
            Ok(w) => w,
            Err(e) => {
                tracing::warn!(error = %e, "workflow watcher failed to start; running without hot reload");
                return;
            }
        };
        while let Some(ev) = watcher.events.recv().await {
            match ev {
                ReloadEvent::Loaded(new_cfg) => {
                    if let Err(e) = new_cfg.validate_for_dispatch() {
                        tracing::warn!(error = %e, "workflow reload failed dispatch validation; keeping last known good");
                        continue;
                    }
                    watcher_handle.reload(Arc::new(*new_cfg)).await;
                    tracing::info!("workflow reloaded");
                }
                ReloadEvent::Failed(e) => {
                    tracing::warn!(error = %e, "workflow reload failed; keeping last known good");
                }
            }
        }
    });

    // Boot the optional HTTP server. SPEC §13.7: bind loopback by default.
    // Also passes the workspace root so the §13.7.3 workspace browser can
    // serve directory listings + file fetches.
    let http_handle = match port_override.or(cfg.server.port) {
        Some(port) => {
            let addr = SocketAddr::from(([127, 0, 0, 1], port));
            let workspace_root = cfg.workspace.root.clone();
            match symphony_http::serve_with_workspace(addr, handle.clone(), workspace_root).await {
                Ok(s) => {
                    tracing::info!(addr = %s.local_addr, "http server listening");
                    Some(s)
                }
                Err(e) => {
                    tracing::warn!(error = %e, "http server failed to bind; continuing without it");
                    None
                }
            }
        }
        None => None,
    };

    // Schedule the immediate first tick.
    handle.tick().await;

    // Wait for shutdown signal.
    if let Err(e) = tokio::signal::ctrl_c().await {
        tracing::warn!(error = %e, "ctrl-c handler failed");
    }
    tracing::info!("shutting down");
    if let Some(s) = http_handle {
        s.shutdown().await;
    }
    handle.shutdown().await;
    let _ = actor_join.await;
    ExitCode::SUCCESS
}
