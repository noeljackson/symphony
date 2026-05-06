// Package watcher implements SPEC §6.2 dynamic reload via filesystem
// notifications. Watch returns a channel that emits one ReloadEvent per
// confirmed change to the workflow file.
//
// Watching the parent directory (rather than the file itself) makes the
// watcher robust against editors that save atomically via rename
// (vim, neovim, JetBrains, etc.) — those produce CREATE / RENAME events
// on the parent, not modify events on the inode the watcher first
// resolved.
//
// Events are debounced (default 250 ms) so a burst of editor saves
// produces one reload, not five.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/noeljackson/symphony/go/internal/config"
)

// DebounceWindow is how long the watcher waits after a fs event before
// firing a reload, so a burst of write-rename-write gets coalesced.
const DebounceWindow = 250 * time.Millisecond

// ReloadEvent is emitted on a confirmed workflow change. Exactly one of
// Definition or Err is non-nil:
//
//   - Definition is set on a successful reload (parsed + validated).
//   - Err is set when the file failed to parse or validate; the caller
//     SHOULD log it and keep the previous config (SPEC §6.2 last-known-
//     good-effective-configuration rule).
type ReloadEvent struct {
	Definition *config.WorkflowDefinition
	Err        error
}

// Watch starts a goroutine that emits ReloadEvents whenever path changes.
// The returned channel is closed when ctx is cancelled or the underlying
// watcher fails fatally.
//
// The supplied path MUST exist; non-existence at start time returns an
// error rather than a silent no-op.
func Watch(ctx context.Context, path string) (<-chan ReloadEvent, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	dir := filepath.Dir(abs)
	target := filepath.Base(abs)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create watcher: %w", err)
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("watch %s: %w", dir, err)
	}

	out := make(chan ReloadEvent, 8)
	go run(ctx, w, abs, target, out)
	return out, nil
}

func run(ctx context.Context, w *fsnotify.Watcher, abs, target string, out chan<- ReloadEvent) {
	defer close(out)
	defer w.Close()

	debounce := time.NewTimer(time.Hour) // long initial delay; resets on event
	if !debounce.Stop() {
		<-debounce.C
	}
	pending := false

	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-w.Events:
			if !open {
				return
			}
			if filepath.Base(ev.Name) != target {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			pending = true
			debounce.Reset(DebounceWindow)
		case err, open := <-w.Errors:
			if !open {
				return
			}
			emit(ctx, out, ReloadEvent{Err: fmt.Errorf("fsnotify: %w", err)})
		case <-debounce.C:
			if !pending {
				continue
			}
			pending = false
			emitReload(ctx, abs, out)
		}
	}
}

func emitReload(ctx context.Context, abs string, out chan<- ReloadEvent) {
	def, err := config.LoadWorkflow(abs)
	if err != nil {
		emit(ctx, out, ReloadEvent{Err: fmt.Errorf("reload %s: %w", abs, err)})
		return
	}
	if err := def.Config.ValidateForDispatch(); err != nil {
		emit(ctx, out, ReloadEvent{Err: fmt.Errorf("validate %s: %w", abs, err)})
		return
	}
	emit(ctx, out, ReloadEvent{Definition: def})
}

func emit(ctx context.Context, out chan<- ReloadEvent, ev ReloadEvent) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
}

// ErrPathMissing is returned by Watch when the workflow path doesn't
// exist at start time. Exposed so callers can distinguish missing-file
// from other watcher errors.
var ErrPathMissing = errors.New("watcher: workflow path does not exist")
