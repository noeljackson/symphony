//! `symphony` binary. SPEC §17.7. Phase 6 wires this to the orchestrator.

use std::path::PathBuf;
use std::process::ExitCode;

use clap::Parser;

#[derive(Parser, Debug)]
#[command(name = "symphony", version, about = "Symphony coding-agent orchestrator")]
struct Cli {
    /// Path to `WORKFLOW.md`. Defaults to `./WORKFLOW.md` in the current
    /// working directory.
    workflow_path: Option<PathBuf>,

    /// Optional HTTP server port. Overrides `server.port` in the workflow.
    /// `0` requests an ephemeral port.
    #[arg(long)]
    port: Option<u16>,
}

fn main() -> ExitCode {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();

    let cli = Cli::parse();
    let path = cli
        .workflow_path
        .unwrap_or_else(|| PathBuf::from("./WORKFLOW.md"));
    if !path.exists() {
        eprintln!("symphony: workflow file not found: {}", path.display());
        return ExitCode::from(2);
    }

    // The runtime + orchestrator wiring lands in Phase 6. For now we just
    // validate that the workflow loads and the typed config is reachable so
    // the binary fails closed rather than silently appearing to start.
    match symphony_core::workflow::WorkflowLoader::load(&path) {
        Ok(def) => match symphony_core::ServiceConfig::from_workflow(&def) {
            Ok(_cfg) => {
                tracing::info!(
                    "symphony: loaded workflow at {} (orchestrator wiring pending)",
                    def.path.display()
                );
                ExitCode::SUCCESS
            }
            Err(e) => {
                eprintln!("symphony: invalid workflow config: {e}");
                ExitCode::FAILURE
            }
        },
        Err(e) => {
            eprintln!("symphony: failed to load workflow: {e}");
            ExitCode::FAILURE
        }
    }
}
