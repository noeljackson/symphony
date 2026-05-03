package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeWorkflow(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func TestLoadWorkflowAppliesDefaults(t *testing.T) {
	path := writeWorkflow(t, "---\n{}\n---\nbody\n")
	def, err := LoadWorkflow(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c := def.Config
	if c.Polling.IntervalMS != 30_000 {
		t.Fatalf("default poll interval: got %d want 30000", c.Polling.IntervalMS)
	}
	if c.Agent.MaxConcurrentAgents != 10 {
		t.Fatalf("default max concurrent: got %d want 10", c.Agent.MaxConcurrentAgents)
	}
	if c.Agent.Backend != BackendCodex {
		t.Fatalf("default backend: got %s want codex", c.Agent.Backend)
	}
	if c.Hooks.TimeoutMS != 60_000 {
		t.Fatalf("default hook timeout: got %d want 60000", c.Hooks.TimeoutMS)
	}
}

func TestLoadWorkflowParsesPerKindLinear(t *testing.T) {
	t.Setenv("SYMPHONY_TEST_LINEAR_KEY", "secret")
	body := `---
tracker:
  kind: linear
linear:
  api_key: $SYMPHONY_TEST_LINEAR_KEY
  project_slug: demo
agent:
  backend: codex
codex:
  command: codex app-server
---
body
`
	def, err := LoadWorkflow(writeWorkflow(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c := def.Config
	if c.Linear.APIKey != "secret" {
		t.Fatalf("api_key indirection: got %q want %q", c.Linear.APIKey, "secret")
	}
	if c.Linear.ProjectSlug != "demo" {
		t.Fatalf("project_slug: got %q want demo", c.Linear.ProjectSlug)
	}
	if c.Tracker.Kind != TrackerLinear {
		t.Fatalf("tracker.kind: got %s want linear", c.Tracker.Kind)
	}
	if err := c.ValidateForDispatch(); err != nil {
		t.Fatalf("ValidateForDispatch: got %v want nil", err)
	}
}

func TestLoadWorkflowParsesPerKindGitHub(t *testing.T) {
	t.Setenv("SYMPHONY_TEST_GH_TOKEN", "ghp_test")
	body := `---
tracker:
  kind: github
github:
  owner: noeljackson
  repo: symphony
  api_token: $SYMPHONY_TEST_GH_TOKEN
  label_priority_map:
    P0: 0
    P1: 1
agent:
  backend: codex
codex:
  command: codex app-server
---
body
`
	def, err := LoadWorkflow(writeWorkflow(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c := def.Config
	if c.Tracker.Kind != TrackerGitHub {
		t.Fatalf("tracker.kind: got %s want github", c.Tracker.Kind)
	}
	if c.GitHub.Owner != "noeljackson" {
		t.Fatalf("owner: got %q want noeljackson", c.GitHub.Owner)
	}
	if c.GitHub.APIToken != "ghp_test" {
		t.Fatalf("api_token indirection: got %q want %q", c.GitHub.APIToken, "ghp_test")
	}
	if got := c.GitHub.LabelPriorityMap["P0"]; got != 0 {
		t.Fatalf("label P0: got %d want 0", got)
	}
	if err := c.ValidateForDispatch(); err != nil {
		t.Fatalf("ValidateForDispatch: got %v want nil", err)
	}
}

func TestValidateRejectsGitHubWithoutAuth(t *testing.T) {
	body := `---
tracker:
  kind: github
github:
  owner: noeljackson
  repo: symphony
agent:
  backend: codex
codex:
  command: codex app-server
---
body
`
	def, err := LoadWorkflow(writeWorkflow(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	err = def.Config.ValidateForDispatch()
	if err == nil {
		t.Fatal("expected ValidateForDispatch to fail without GitHub auth")
	}
	cerr, ok := err.(*ConfigError)
	if !ok || cerr.Code != ErrMissingGitHubAuth {
		t.Fatalf("error: got %v want code=%s", err, ErrMissingGitHubAuth)
	}
}

func TestValidateRejectsZeroOrNegativeDailyBudget(t *testing.T) {
	for _, body := range []string{
		"---\nagent:\n  daily_budget_usd: 0\n---\nbody\n",
		"---\nagent:\n  daily_budget_usd: -1.0\n---\nbody\n",
	} {
		_, err := LoadWorkflow(writeWorkflow(t, body))
		if err == nil {
			t.Fatalf("expected load to fail for %q", body)
		}
		cerr, ok := err.(*ConfigError)
		if !ok || cerr.Code != ErrInvalidValue || cerr.Field != "agent.daily_budget_usd" {
			t.Fatalf("error: got %v want code=%s field=agent.daily_budget_usd", err, ErrInvalidValue)
		}
	}
}

func TestValidateRejectsUnknownBackend(t *testing.T) {
	body := `---
tracker:
  kind: linear
linear:
  api_key: x
  project_slug: demo
agent:
  backend: jiraagent
---
body
`
	def, err := LoadWorkflow(writeWorkflow(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	err = def.Config.ValidateForDispatch()
	cerr, ok := err.(*ConfigError)
	if !ok || cerr.Code != ErrUnsupportedAgentBackend {
		t.Fatalf("error: got %v want code=%s", err, ErrUnsupportedAgentBackend)
	}
}
