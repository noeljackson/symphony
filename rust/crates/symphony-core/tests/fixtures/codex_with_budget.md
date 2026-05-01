---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: my-project
agent:
  backend: codex
  daily_budget_usd: 5.00
codex:
  command: codex app-server
---

You are working on an issue from Linear: {{ issue.identifier }}.
