// Package workspace owns the per-issue filesystem lifecycle defined in
// SPEC §8 / §9: sanitize an issue identifier into a workspace key, ensure
// the workspace directory exists under the configured root, and run the
// `after_create` / `before_run` / `after_run` / `before_remove` hooks at
// the right phases with timeouts.
//
// Every method enforces SPEC §9.5 path-safety: the resolved workspace
// path MUST live under the configured root (no `..` traversal, no
// symlink escape). Violations return ErrEscapedRoot.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrEscapedRoot is returned when a sanitized workspace path would
// resolve outside the configured root.
var ErrEscapedRoot = errors.New("workspace path escaped configured root")

// HookKind identifies which phase of the workspace lifecycle is running.
type HookKind string

const (
	HookAfterCreate  HookKind = "after_create"
	HookBeforeRun    HookKind = "before_run"
	HookAfterRun     HookKind = "after_run"
	HookBeforeRemove HookKind = "before_remove"
)

// Hooks bundles the four optional lifecycle scripts.
type Hooks struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	Timeout      time.Duration
}

// Manager owns one workflow's workspace root and hook scripts.
type Manager struct {
	root  string
	hooks Hooks
}

// New constructs a Manager. The supplied root is canonicalized; the
// directory is created on first use rather than at construction time so
// `New` is side-effect-free.
func New(root string, hooks Hooks) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("workspace.New: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	return &Manager{root: filepath.Clean(abs), hooks: hooks}, nil
}

// Root returns the canonical workspace root.
func (m *Manager) Root() string { return m.root }

// SanitizeIdentifier converts an issue identifier to a workspace key per
// SPEC §4.2: any character not in [A-Za-z0-9._-] becomes `_`.
func SanitizeIdentifier(identifier string) string {
	if identifier == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(identifier))
	for _, r := range identifier {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// PathFor returns the absolute filesystem path for `identifier`. The path
// is enforced to live under the configured root.
func (m *Manager) PathFor(identifier string) (string, error) {
	key := SanitizeIdentifier(identifier)
	candidate := filepath.Clean(filepath.Join(m.root, key))
	if !pathIsUnder(m.root, candidate) {
		return "", fmt.Errorf("%w: %s -> %s", ErrEscapedRoot, identifier, candidate)
	}
	return candidate, nil
}

// EnsureCreated guarantees the workspace directory exists. When it didn't
// before, the after_create hook fires inside the directory; failures are
// fatal — the partial directory is removed so a retry starts clean.
//
// Returns the absolute workspace path, plus a boolean indicating whether
// the directory was newly created on this call (true) or already existed
// (false).
func (m *Manager) EnsureCreated(ctx context.Context, identifier string) (string, bool, error) {
	path, err := m.PathFor(identifier)
	if err != nil {
		return "", false, err
	}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() {
			return "", false, fmt.Errorf("workspace: %s exists and is not a directory", path)
		}
		return path, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("stat workspace: %w", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", false, fmt.Errorf("create workspace: %w", err)
	}
	if m.hooks.AfterCreate != "" {
		if err := m.RunHook(ctx, HookAfterCreate, path, identifier); err != nil {
			_ = os.RemoveAll(path)
			return "", false, fmt.Errorf("after_create hook failed: %w", err)
		}
	}
	return path, true, nil
}

// Remove deletes the workspace directory after firing the before_remove
// hook (best-effort: the hook's failure is logged but doesn't block
// cleanup).
func (m *Manager) Remove(ctx context.Context, identifier string) error {
	path, err := m.PathFor(identifier)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat workspace: %w", err)
	}
	if m.hooks.BeforeRemove != "" {
		// Best-effort per SPEC §5.3.4. We swallow the error.
		_ = m.RunHook(ctx, HookBeforeRemove, path, identifier)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove workspace: %w", err)
	}
	return nil
}

// RunHook executes one hook script with the given workspace path as cwd
// and `SYMPHONY_*` env vars exported. Returns nil when the script is
// unset or completes within the timeout.
//
// Per SPEC §5.3.4:
//   - after_create / before_run failures abort the current attempt.
//   - after_run / before_remove failures are logged + swallowed by the
//     caller (Manager.Remove handles before_remove that way; the runtime
//     is responsible for after_run).
func (m *Manager) RunHook(ctx context.Context, kind HookKind, path, identifier string) error {
	script := m.scriptFor(kind)
	if script == "" {
		return nil
	}
	timeout := m.hooks.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(hctx, "bash", "-lc", script)
	cmd.Dir = path
	cmd.Env = append(os.Environ(),
		"SYMPHONY_WORKSPACE="+path,
		"SYMPHONY_ISSUE_IDENTIFIER="+identifier,
		"SYMPHONY_HOOK="+string(kind),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("hook %s: %w (output: %s)", kind, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Manager) scriptFor(kind HookKind) string {
	switch kind {
	case HookAfterCreate:
		return m.hooks.AfterCreate
	case HookBeforeRun:
		return m.hooks.BeforeRun
	case HookAfterRun:
		return m.hooks.AfterRun
	case HookBeforeRemove:
		return m.hooks.BeforeRemove
	}
	return ""
}

// pathIsUnder reports whether candidate is the same as parent or lives
// strictly underneath it (per [filepath.Clean] semantics).
func pathIsUnder(parent, candidate string) bool {
	rel, err := filepath.Rel(parent, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
