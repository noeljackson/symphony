// Package config defines ServiceConfig (the typed view of WORKFLOW.md) per
// SPEC v3 §4.1.3 / §6, plus parsing and validation helpers.
package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// AgentBackend selects which agent runner to dispatch through (SPEC §5.3.5).
type AgentBackend string

const (
	BackendCodex             AgentBackend = "codex"
	BackendClaudeCode        AgentBackend = "claude_code"
	BackendOpenAICompat      AgentBackend = "openai_compat"
	BackendAnthropicMessages AgentBackend = "anthropic_messages"
)

// ParseBackend returns the typed selector for a raw string. Unknown values
// are returned as-is so dispatch preflight can produce a useful error.
func ParseBackend(raw string) AgentBackend {
	return AgentBackend(strings.TrimSpace(raw))
}

// IsKnown reports whether the backend is one of the v3 core conformance values.
func (b AgentBackend) IsKnown() bool {
	switch b {
	case BackendCodex, BackendClaudeCode, BackendOpenAICompat, BackendAnthropicMessages:
		return true
	}
	return false
}

// TrackerKind selects the tracker adapter (SPEC §5.3.1).
type TrackerKind string

const (
	TrackerLinear TrackerKind = "linear"
	TrackerGitHub TrackerKind = "github"
)

// TrackerConfig holds the SPEC v3 §5.3.1 shared tracker keys. Per-kind
// auth/endpoint blocks live in LinearConfig / GitHubConfig.
type TrackerConfig struct {
	Kind           TrackerKind
	ActiveStates   []string
	TerminalStates []string
}

// LinearConfig is populated when TrackerConfig.Kind == TrackerLinear (§5.3.1.A).
type LinearConfig struct {
	Endpoint    string
	APIKey      string
	ProjectSlug string
}

// GitHubConfig is populated when TrackerConfig.Kind == TrackerGitHub (§5.3.1.B).
type GitHubConfig struct {
	Endpoint          string
	Owner             string
	Repo              string
	APIToken          string
	AppID             string
	AppInstallationID string
	PrivateKey        string
	LabelPriorityMap  map[string]int
	Assignee          string
}

// PollingConfig holds SPEC §5.3.2 polling settings.
type PollingConfig struct {
	IntervalMS uint64
}

// WorkspaceConfig holds SPEC §5.3.3 workspace settings.
type WorkspaceConfig struct {
	Root string
}

// HooksConfig holds SPEC §5.3.4 hook scripts and shared timeout.
type HooksConfig struct {
	AfterCreate  *string
	BeforeRun    *string
	AfterRun     *string
	BeforeRemove *string
	TimeoutMS    uint64
}

// AgentConfig holds SPEC v2 §5.3.5 agent-level config + v3 daily budget cap.
type AgentConfig struct {
	Backend                    AgentBackend
	MaxConcurrentAgents        int
	MaxTurns                   uint32
	MaxRetryBackoffMS          uint64
	MaxConcurrentAgentsByState map[string]int
	// DailyBudgetUSD is nil when no cap is set; a positive value caps cumulative
	// agent USD cost per UTC calendar day (§5.3.5 / §13.5).
	DailyBudgetUSD *float64
}

// CodexConfig holds SPEC §5.3.6.A codex backend config. Pass-through values
// for approval_policy / thread_sandbox / turn_sandbox_policy are kept as raw
// YAML.
type CodexConfig struct {
	Command           string
	ApprovalPolicy    any
	ThreadSandbox     any
	TurnSandboxPolicy any
	TurnTimeoutMS     uint64
	ReadTimeoutMS     uint64
	StallTimeoutMS    int64
}

// ClaudeCodeConfig holds SPEC §5.3.6.B claude_code backend config.
type ClaudeCodeConfig struct {
	Command         string
	PermissionMode  string
	AllowedTools    []string
	DisallowedTools []string
	Model           string
	TurnTimeoutMS   uint64
	ReadTimeoutMS   uint64
	StallTimeoutMS  int64
}

// OpenAICompatConfig holds SPEC §5.3.6.C openai_compat backend config.
type OpenAICompatConfig struct {
	Endpoint  string
	APIKey    string
	Model     string
	MaxTokens uint32
	System    string
}

// AnthropicMessagesConfig holds SPEC §5.3.6.D anthropic_messages backend config.
type AnthropicMessagesConfig struct {
	Endpoint  string
	APIKey    string
	Model     string
	MaxTokens uint32
	System    string
}

// ServerConfig enables the OPTIONAL §13.7 HTTP server. Port=nil disables it;
// Port=&0 requests an ephemeral bind for tests.
type ServerConfig struct {
	Port *uint16
}

// ServiceConfig is the typed view of one workflow's front matter.
type ServiceConfig struct {
	Tracker           TrackerConfig
	Linear            LinearConfig
	GitHub            GitHubConfig
	Polling           PollingConfig
	Workspace         WorkspaceConfig
	Hooks             HooksConfig
	Agent             AgentConfig
	Codex             CodexConfig
	ClaudeCode        ClaudeCodeConfig
	OpenAICompat      OpenAICompatConfig
	AnthropicMessages AnthropicMessagesConfig
	Server            ServerConfig

	// Raw is the original front-matter map for forward-compatibility / extensions.
	Raw map[string]any

	// WorkflowPath records the loaded WORKFLOW.md location for relative-path
	// resolution and reload semantics.
	WorkflowPath string
}

// HookTimeout returns the configured hook timeout as a time.Duration.
func (c *ServiceConfig) HookTimeout() time.Duration {
	return time.Duration(c.Hooks.TimeoutMS) * time.Millisecond
}

// PollInterval returns the configured poll interval as a time.Duration.
func (c *ServiceConfig) PollInterval() time.Duration {
	return time.Duration(c.Polling.IntervalMS) * time.Millisecond
}

// WorkflowDir returns the directory containing the loaded WORKFLOW.md.
func (c *ServiceConfig) WorkflowDir() string {
	if c.WorkflowPath == "" {
		return "."
	}
	return filepath.Dir(c.WorkflowPath)
}

// ConfigError is returned by Load and ValidateForDispatch.
type ConfigError struct {
	Code    string
	Field   string
	Message string
}

func (e *ConfigError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s (field: %s): %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Sentinel error codes per SPEC §6.3.
const (
	ErrMissingTrackerAPIKey      = "missing_tracker_api_key"
	ErrMissingTrackerProjectSlug = "missing_tracker_project_slug"
	ErrMissingGitHubOwner        = "missing_github_owner"
	ErrMissingGitHubRepo         = "missing_github_repo"
	ErrMissingGitHubAuth         = "missing_github_auth"
	ErrUnsupportedTrackerKind    = "unsupported_tracker_kind"
	ErrEmptyCodexCommand         = "empty_codex_command"
	ErrEmptyClaudeCodeCommand    = "empty_claude_code_command"
	ErrUnimplementedAgentBackend = "unimplemented_agent_backend"
	ErrUnsupportedAgentBackend   = "unsupported_agent_backend"
	ErrInvalidValue              = "invalid_value"
)
