# Symphony — Go reference implementation

This is the canonical implementation of [`SPEC.md`](../SPEC.md) (v3 and
forward). Earlier ports (`elixir/`, `rust/`) remain as historical
references.

## Layout

```
go/
  cmd/symphony/      # CLI binary
  internal/
    config/          # WORKFLOW.md loader + ServiceConfig
    dispatch/        # Pure dispatch eligibility + sorting + retry math
    issue/           # Issue / Blocker types
    orchestrator/    # Single-authority actor + WorkerRunner contract
    state/           # OrchestratorState + cost ledger + recent_events
    store/           # StateStore interface (Postgres impl follows)
    tracker/         # Tracker interface + memory test helper
```

## Workflow

```
cd go
make fmt-check
make vet
make test
```

See [../AGENTS.md](../AGENTS.md) for spec-first contributor rules. Behavior
changes land in `SPEC.md` first; this tree implements against the merged spec.
