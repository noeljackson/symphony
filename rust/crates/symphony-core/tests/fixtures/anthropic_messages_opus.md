---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: my-project
agent:
  backend: anthropic_messages
anthropic_messages:
  api_key: $ANTHROPIC_API_KEY
  model: claude-opus-4-7
  max_tokens: 8192
  system: |
    You are a senior engineer working on the Symphony repo.
---

{{ issue.title }}
