package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkflowDefinition is the parsed view of a WORKFLOW.md: typed front matter
// plus the Liquid template body. The template is kept as a raw string; the
// agent-backend layer applies Liquid rendering at dispatch time.
type WorkflowDefinition struct {
	Path           string
	Config         *ServiceConfig
	PromptTemplate string
}

// LoadWorkflow reads a WORKFLOW.md from disk and parses it into a
// [WorkflowDefinition]. The file MUST have YAML front matter delimited by
// `---` lines; everything after the second delimiter is the prompt template.
func LoadWorkflow(path string) (*WorkflowDefinition, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow path: %w", err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read workflow file: %w", err)
	}
	frontMatter, body, err := splitFrontMatter(raw)
	if err != nil {
		return nil, err
	}
	var rawMap map[string]any
	if len(frontMatter) > 0 {
		if err := yaml.Unmarshal(frontMatter, &rawMap); err != nil {
			return nil, fmt.Errorf("parse front matter YAML: %w", err)
		}
	} else {
		rawMap = map[string]any{}
	}
	cfg, err := parseConfig(rawMap, abs)
	if err != nil {
		return nil, err
	}
	return &WorkflowDefinition{
		Path:           abs,
		Config:         cfg,
		PromptTemplate: string(body),
	}, nil
}

func splitFrontMatter(raw []byte) ([]byte, []byte, error) {
	delim := []byte("---")
	// Skip optional leading whitespace / UTF-8 BOM.
	rest := raw
	if len(rest) >= 3 && rest[0] == 0xEF && rest[1] == 0xBB && rest[2] == 0xBF {
		rest = rest[3:]
	}
	rest = bytes.TrimLeft(rest, " \t\r\n")
	if !bytes.HasPrefix(rest, delim) {
		// No front matter — entire file is prompt body.
		return nil, raw, nil
	}
	rest = rest[len(delim):]
	rest = bytes.TrimLeft(rest, "\r\n")
	end := bytes.Index(rest, append([]byte("\n"), delim...))
	if end == -1 {
		return nil, nil, fmt.Errorf("workflow front matter is not terminated by a closing `---`")
	}
	front := rest[:end]
	after := rest[end+1+len(delim):]
	after = bytes.TrimLeft(after, "\r\n")
	return front, after, nil
}

func parseConfig(raw map[string]any, workflowPath string) (*ServiceConfig, error) {
	cfg := &ServiceConfig{
		Raw:          raw,
		WorkflowPath: workflowPath,
	}

	tracker, err := parseTracker(getMap(raw, "tracker"))
	if err != nil {
		return nil, err
	}
	cfg.Tracker = tracker

	cfg.Linear = parseLinear(getMap(raw, "linear"))
	cfg.GitHub = parseGitHub(getMap(raw, "github"))
	cfg.Polling = parsePolling(getMap(raw, "polling"))
	cfg.Workspace = parseWorkspace(getMap(raw, "workspace"), filepath.Dir(workflowPath))
	cfg.Hooks = parseHooks(getMap(raw, "hooks"))

	agent, err := parseAgent(getMap(raw, "agent"))
	if err != nil {
		return nil, err
	}
	cfg.Agent = agent

	cfg.Codex = parseCodex(getMap(raw, "codex"))
	cfg.ClaudeCode = parseClaudeCode(getMap(raw, "claude_code"))
	cfg.OpenAICompat = parseOpenAICompat(getMap(raw, "openai_compat"))
	cfg.AnthropicMessages = parseAnthropicMessages(getMap(raw, "anthropic_messages"))
	cfg.Server = parseServer(getMap(raw, "server"))

	return cfg, nil
}

func getMap(raw map[string]any, key string) map[string]any {
	v, ok := raw[key]
	if !ok || v == nil {
		return map[string]any{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

func getString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func getStringPtr(m map[string]any, key string) *string {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}

func getStringList(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func getUint64(m map[string]any, key string, defaultV uint64) uint64 {
	v, ok := m[key]
	if !ok || v == nil {
		return defaultV
	}
	switch n := v.(type) {
	case int:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case int64:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case uint64:
		return n
	case float64:
		if n < 0 {
			return 0
		}
		return uint64(n)
	}
	return defaultV
}

func getInt64(m map[string]any, key string, defaultV int64) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return defaultV
	}
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return defaultV
}

// resolveSecret expands a `$VAR_NAME` indirection per SPEC §5.3.1. Literal
// values pass through unchanged. Empty `$VAR` resolutions return "".
func resolveSecret(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "$") || len(s) < 2 {
		return raw
	}
	return os.Getenv(s[1:])
}

func parseTracker(m map[string]any) (TrackerConfig, error) {
	kindRaw := getString(m, "kind")
	if kindRaw == "" {
		kindRaw = "linear"
	}
	t := TrackerConfig{
		Kind:           TrackerKind(strings.ToLower(strings.TrimSpace(kindRaw))),
		ActiveStates:   getStringList(m, "active_states"),
		TerminalStates: getStringList(m, "terminal_states"),
	}
	if len(t.ActiveStates) == 0 {
		t.ActiveStates = []string{"Todo", "In Progress"}
	}
	if len(t.TerminalStates) == 0 {
		t.TerminalStates = []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}
	}
	return t, nil
}

func parseLinear(m map[string]any) LinearConfig {
	endpoint := getString(m, "endpoint")
	if endpoint == "" {
		endpoint = "https://api.linear.app/graphql"
	}
	return LinearConfig{
		Endpoint:    endpoint,
		APIKey:      resolveSecret(getString(m, "api_key")),
		ProjectSlug: getString(m, "project_slug"),
	}
}

func parseGitHub(m map[string]any) GitHubConfig {
	endpoint := getString(m, "endpoint")
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	out := GitHubConfig{
		Endpoint:          endpoint,
		Owner:             getString(m, "owner"),
		Repo:              getString(m, "repo"),
		APIToken:          resolveSecret(getString(m, "api_token")),
		AppID:             getString(m, "app_id"),
		AppInstallationID: getString(m, "app_installation_id"),
		PrivateKey:        resolveSecret(getString(m, "private_key")),
		Assignee:          getString(m, "assignee"),
	}
	if raw, ok := m["label_priority_map"].(map[string]any); ok {
		out.LabelPriorityMap = make(map[string]int, len(raw))
		for k, v := range raw {
			switch n := v.(type) {
			case int:
				out.LabelPriorityMap[k] = n
			case int64:
				out.LabelPriorityMap[k] = int(n)
			case float64:
				out.LabelPriorityMap[k] = int(n)
			}
		}
	}
	return out
}

func parsePolling(m map[string]any) PollingConfig {
	return PollingConfig{IntervalMS: getUint64(m, "interval_ms", 30_000)}
}

func parseWorkspace(m map[string]any, workflowDir string) WorkspaceConfig {
	root := getString(m, "root")
	if root == "" {
		return WorkspaceConfig{Root: filepath.Join(os.TempDir(), "symphony_workspaces")}
	}
	expanded := expandUserHome(root)
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(workflowDir, expanded)
	}
	return WorkspaceConfig{Root: filepath.Clean(expanded)}
}

func expandUserHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func parseHooks(m map[string]any) HooksConfig {
	return HooksConfig{
		AfterCreate:  getStringPtr(m, "after_create"),
		BeforeRun:    getStringPtr(m, "before_run"),
		AfterRun:     getStringPtr(m, "after_run"),
		BeforeRemove: getStringPtr(m, "before_remove"),
		TimeoutMS:    getUint64(m, "timeout_ms", 60_000),
	}
}

func parseAgent(m map[string]any) (AgentConfig, error) {
	backendRaw := getString(m, "backend")
	if backendRaw == "" {
		backendRaw = string(BackendCodex)
	}
	maxTurns := getUint64(m, "max_turns", 20)
	if v, ok := m["max_turns"]; ok && v != nil {
		if n := getUint64(m, "max_turns", 0); n == 0 {
			return AgentConfig{}, &ConfigError{
				Code:    ErrInvalidValue,
				Field:   "agent.max_turns",
				Message: "must be a positive integer",
			}
		}
	}
	out := AgentConfig{
		Backend:                    ParseBackend(backendRaw),
		MaxConcurrentAgents:        int(getUint64(m, "max_concurrent_agents", 10)),
		MaxTurns:                   uint32(maxTurns),
		MaxRetryBackoffMS:          getUint64(m, "max_retry_backoff_ms", 300_000),
		MaxConcurrentAgentsByState: map[string]int{},
	}
	if raw, ok := m["max_concurrent_agents_by_state"].(map[string]any); ok {
		for k, v := range raw {
			n := 0
			switch x := v.(type) {
			case int:
				n = x
			case int64:
				n = int(x)
			case float64:
				n = int(x)
			}
			if n > 0 {
				out.MaxConcurrentAgentsByState[strings.ToLower(k)] = n
			}
		}
	}
	if v, ok := m["daily_budget_usd"]; ok && v != nil {
		f, valid := toFloat64(v)
		if !valid || f <= 0 || isNotFinite(f) {
			return AgentConfig{}, &ConfigError{
				Code:    ErrInvalidValue,
				Field:   "agent.daily_budget_usd",
				Message: "must be a positive number",
			}
		}
		out.DailyBudgetUSD = &f
	}
	return out, nil
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func isNotFinite(f float64) bool {
	return f != f || f > 1.0e308 || f < -1.0e308
}

func parseCodex(m map[string]any) CodexConfig {
	cmd := getString(m, "command")
	if cmd == "" {
		cmd = "codex app-server"
	}
	return CodexConfig{
		Command:           cmd,
		ApprovalPolicy:    m["approval_policy"],
		ThreadSandbox:     m["thread_sandbox"],
		TurnSandboxPolicy: m["turn_sandbox_policy"],
		TurnTimeoutMS:     getUint64(m, "turn_timeout_ms", 3_600_000),
		ReadTimeoutMS:     getUint64(m, "read_timeout_ms", 5_000),
		StallTimeoutMS:    getInt64(m, "stall_timeout_ms", 300_000),
	}
}

func parseClaudeCode(m map[string]any) ClaudeCodeConfig {
	cmd := getString(m, "command")
	if cmd == "" {
		cmd = "claude --print --output-format stream-json --input-format stream-json --verbose"
	}
	return ClaudeCodeConfig{
		Command:         cmd,
		PermissionMode:  getString(m, "permission_mode"),
		AllowedTools:    getStringList(m, "allowed_tools"),
		DisallowedTools: getStringList(m, "disallowed_tools"),
		Model:           getString(m, "model"),
		TurnTimeoutMS:   getUint64(m, "turn_timeout_ms", 3_600_000),
		ReadTimeoutMS:   getUint64(m, "read_timeout_ms", 5_000),
		StallTimeoutMS:  getInt64(m, "stall_timeout_ms", 300_000),
	}
}

func parseOpenAICompat(m map[string]any) OpenAICompatConfig {
	return OpenAICompatConfig{
		Endpoint:  getString(m, "endpoint"),
		APIKey:    resolveSecret(getString(m, "api_key")),
		Model:     getString(m, "model"),
		MaxTokens: uint32(getUint64(m, "max_tokens", 0)),
		System:    getString(m, "system"),
	}
}

func parseAnthropicMessages(m map[string]any) AnthropicMessagesConfig {
	return AnthropicMessagesConfig{
		Endpoint:  getString(m, "endpoint"),
		APIKey:    resolveSecret(getString(m, "api_key")),
		Model:     getString(m, "model"),
		MaxTokens: uint32(getUint64(m, "max_tokens", 0)),
		System:    getString(m, "system"),
	}
}

func parseServer(m map[string]any) ServerConfig {
	v, ok := m["port"]
	if !ok || v == nil {
		return ServerConfig{}
	}
	switch n := v.(type) {
	case int:
		if n < 0 || n > 65535 {
			return ServerConfig{}
		}
		port := uint16(n)
		return ServerConfig{Port: &port}
	case int64:
		if n < 0 || n > 65535 {
			return ServerConfig{}
		}
		port := uint16(n)
		return ServerConfig{Port: &port}
	}
	return ServerConfig{}
}
