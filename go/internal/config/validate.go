package config

import "strings"

// ValidateForDispatch implements the SPEC §6.3 dispatch preflight check.
//
// Returns nil when the config is dispatch-ready; otherwise returns a
// *ConfigError naming the first failure. Callers SHOULD log validation
// errors with `issue_id`/`issue_identifier` context where available.
func (c *ServiceConfig) ValidateForDispatch() error {
	switch c.Tracker.Kind {
	case TrackerLinear:
		if strings.TrimSpace(c.Linear.APIKey) == "" {
			return &ConfigError{Code: ErrMissingTrackerAPIKey, Field: "linear.api_key"}
		}
		if strings.TrimSpace(c.Linear.ProjectSlug) == "" {
			return &ConfigError{Code: ErrMissingTrackerProjectSlug, Field: "linear.project_slug"}
		}
	case TrackerGitHub:
		if strings.TrimSpace(c.GitHub.Owner) == "" {
			return &ConfigError{Code: ErrMissingGitHubOwner, Field: "github.owner"}
		}
		if strings.TrimSpace(c.GitHub.Repo) == "" {
			return &ConfigError{Code: ErrMissingGitHubRepo, Field: "github.repo"}
		}
		hasPAT := strings.TrimSpace(c.GitHub.APIToken) != ""
		hasApp := strings.TrimSpace(c.GitHub.AppID) != "" &&
			strings.TrimSpace(c.GitHub.AppInstallationID) != "" &&
			strings.TrimSpace(c.GitHub.PrivateKey) != ""
		if !hasPAT && !hasApp {
			return &ConfigError{
				Code:    ErrMissingGitHubAuth,
				Field:   "github.api_token",
				Message: "set api_token, or app_id + app_installation_id + private_key",
			}
		}
	default:
		return &ConfigError{
			Code:    ErrUnsupportedTrackerKind,
			Field:   "tracker.kind",
			Message: string(c.Tracker.Kind),
		}
	}

	switch c.Agent.Backend {
	case BackendCodex:
		if strings.TrimSpace(c.Codex.Command) == "" {
			return &ConfigError{Code: ErrEmptyCodexCommand, Field: "codex.command"}
		}
	case BackendClaudeCode:
		if strings.TrimSpace(c.ClaudeCode.Command) == "" {
			return &ConfigError{Code: ErrEmptyClaudeCodeCommand, Field: "claude_code.command"}
		}
	case BackendOpenAICompat, BackendAnthropicMessages:
		// Reserved for the per-backend impl PR.
		return &ConfigError{
			Code:    ErrUnimplementedAgentBackend,
			Field:   "agent.backend",
			Message: string(c.Agent.Backend),
		}
	default:
		return &ConfigError{
			Code:    ErrUnsupportedAgentBackend,
			Field:   "agent.backend",
			Message: string(c.Agent.Backend),
		}
	}
	return nil
}
