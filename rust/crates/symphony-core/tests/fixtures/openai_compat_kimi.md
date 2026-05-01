---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: my-project
agent:
  backend: openai_compat
openai_compat:
  endpoint: https://api.moonshot.ai/v1
  api_key: $MOONSHOT_API_KEY
  model: kimi-k2
  max_tokens: 8192
---

{{ issue.title }}
