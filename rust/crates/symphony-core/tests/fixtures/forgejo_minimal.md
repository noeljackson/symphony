---
tracker:
  kind: forgejo
  active_states: [ready, in-progress]
  terminal_states: [done, closed]
forgejo:
  endpoint: https://forge.example/api/v1
  owner: acme
  repo: widgets
  api_token: $FORGEJO_TOKEN
  label_priority_map:
    P0: 0
agent:
  backend: codex
---
Work on {{ issue.identifier }}: {{ issue.title }}.
