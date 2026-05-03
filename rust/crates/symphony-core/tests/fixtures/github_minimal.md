---
tracker:
  kind: github
  active_states:
    - ready
    - in-progress
  terminal_states:
    - done
    - closed
github:
  owner: noeljackson
  repo: symphony
  api_token: $GITHUB_TOKEN
  label_priority_map:
    P0: 0
    P1: 1
    P2: 2
agent:
  backend: codex
codex:
  command: codex app-server
---

You are working on a GitHub issue: {{ issue.identifier }}.
