---
tracker:
  kind: linear
  endpoint: https://api.linear.app/graphql
  api_key: $LINEAR_API_KEY
  project_slug: my-project
  active_states:
    - Todo
    - In Progress
  terminal_states:
    - Done
    - Cancelled
polling:
  interval_ms: 30000
workspace:
  root: ~/symphony-workspaces
hooks:
  after_create: |
    git clone https://example.com/repo.git .
  before_run: |
    npm install
  timeout_ms: 60000
agent:
  backend: claude_code
  max_concurrent_agents: 4
  max_turns: 30
  max_concurrent_agents_by_state:
    todo: 2
    in progress: 4
claude_code:
  command: claude --print --output-format stream-json --input-format stream-json --verbose
  permission_mode: bypassPermissions
  allowed_tools:
    - Bash
    - Edit
  model: claude-opus-4-7
server:
  port: 0
---

Build a feature for {{ issue.identifier }}: {{ issue.title }}.
