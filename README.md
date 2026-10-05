# Symphony

Symphony turns project work into isolated, autonomous implementation runs, allowing teams to manage
work instead of supervising coding agents.

[![Symphony demo video preview](.github/media/symphony-demo-poster.jpg)](.github/media/symphony-demo.mp4)

_In this [demo video](.github/media/symphony-demo.mp4), Symphony monitors a Linear board for work and spawns agents to handle the tasks. The agents complete the tasks and provide proof of work: CI status, PR review feedback, complexity analysis, and walkthrough videos. When accepted, the agents land the PR safely. Engineers do not need to supervise individual coding agents; they can manage the work at a higher level._

> [!WARNING]
> Symphony is a low-key engineering preview for testing in trusted environments.

## Agent backends

As of [SPEC v2](SPEC.md), Symphony is backend-agnostic. A workflow selects
its agent runner via `agent.backend` in `WORKFLOW.md`:

| `agent.backend` | What it drives | Transport |
|---|---|---|
| `codex` | OpenAI Codex `app-server` | Subprocess, JSON-RPC over stdio |
| `claude_code` | Anthropic Claude Code CLI in stream-json mode | Subprocess, JSON over stdio |
| `openai_compat` | Any OpenAI-compatible chat-completions endpoint — OpenAI, Moonshot Kimi K2, Zhipu GLM, DeepSeek, vLLM, llama.cpp servers | In-process HTTP |
| `anthropic_messages` | Anthropic Messages API | In-process HTTP |

See [SPEC §10](SPEC.md#10-agent-runner-protocol) for the full per-backend
contract and [SPEC §5.3.6](SPEC.md#536-backend-specific-configuration-blocks)
for the configuration schema. Implementations MAY support a subset of
backends and MUST document which.

## Running Symphony

### Requirements

Symphony works best in codebases that have adopted
[harness engineering](https://openai.com/index/harness-engineering/). Symphony is the next step --
moving from managing coding agents to managing work that needs to get done.

### Option 1. Make your own

Tell your favorite coding agent to build Symphony in a programming language of your choice:

> Implement Symphony according to the following spec:
> https://github.com/openai/symphony/blob/main/SPEC.md

### Option 2. Use the Rust implementation

This repository ships the [Rust implementation](rust/README.md), which
targets SPEC v2 with the Codex and Claude Code backends, an HTTP dashboard,
and the `linear_graphql` extension. It also implements the [SPEC v3](SPEC.md)
GitHub Issues and Forgejo Issues tracker contracts. Persistent storage
remains unimplemented.

```sh
cd rust
cargo build --release --locked
cargo test --workspace --locked
```

Configure a `WORKFLOW.md` for Linear, GitHub Issues, or Forgejo Issues as
documented in [rust/README.md](rust/README.md), then run `rust/target/release/symphony`
with that workflow's path.

You can also ask your favorite coding agent to help with the setup:

> Set up Symphony for my repository based on
> https://github.com/noeljackson/symphony/blob/main/rust/README.md

---

## License

This project is licensed under the [Apache License 2.0](LICENSE).
