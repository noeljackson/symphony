// Package prompt renders the Liquid prompt template for a worker session
// per SPEC §5.4. Template variables: `issue.{...}` (always) and
// `attempt.{number}` (when a retry attempt is being dispatched).
//
// The renderer uses strict variable / filter mode so typos surface as
// errors rather than silently producing empty strings.
package prompt

import (
	"fmt"

	"github.com/osteele/liquid"

	"github.com/noeljackson/symphony/go/internal/issue"
)

// Builder renders one workflow's prompt template against per-attempt
// context. Construct once at startup; Render is goroutine-safe (the
// underlying Liquid engine has no mutable state).
type Builder struct {
	tmpl *liquid.Template
}

// New parses the supplied template body. Returns an error when the
// template fails to parse; the message includes the line/col of the
// first syntax error.
func New(body string) (*Builder, error) {
	engine := liquid.NewEngine()
	engine.Delims("{{", "}}", "{%", "%}")
	tmpl, err := engine.ParseTemplate([]byte(body))
	if err != nil {
		return nil, fmt.Errorf("parse prompt template: %w", err)
	}
	return &Builder{tmpl: tmpl}, nil
}

// Render produces the prompt for one agent attempt. `attempt` is nil for
// the first dispatch; subsequent attempts pass the attempt counter so
// templates can render retry-specific guidance.
func (b *Builder) Render(i issue.Issue, attempt *uint32) (string, error) {
	bindings := map[string]any{
		"issue":   issueBindings(i),
		"attempt": attemptBindings(attempt),
	}
	out, err := b.tmpl.Render(bindings)
	if err != nil {
		return "", fmt.Errorf("render prompt: %w", err)
	}
	return string(out), nil
}

func issueBindings(i issue.Issue) map[string]any {
	out := map[string]any{
		"id":         i.ID,
		"identifier": i.Identifier,
		"title":      i.Title,
		"state":      i.State,
		"labels":     i.Labels,
	}
	if i.Description != nil {
		out["description"] = *i.Description
	} else {
		out["description"] = ""
	}
	if i.Priority != nil {
		out["priority"] = *i.Priority
	}
	if i.BranchName != nil {
		out["branch_name"] = *i.BranchName
	}
	if i.URL != nil {
		out["url"] = *i.URL
	}
	if len(i.BlockedBy) > 0 {
		blockers := make([]map[string]any, 0, len(i.BlockedBy))
		for _, b := range i.BlockedBy {
			blockers = append(blockers, map[string]any{
				"id":         b.ID,
				"identifier": b.Identifier,
				"state":      b.State,
			})
		}
		out["blocked_by"] = blockers
	}
	return out
}

func attemptBindings(attempt *uint32) map[string]any {
	if attempt == nil {
		return map[string]any{"number": 0}
	}
	return map[string]any{"number": int(*attempt)}
}
