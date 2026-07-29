//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris || windows

package yaml

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/GizClaw/flowcraft/sdkx/inference/config"
	"github.com/fsnotify/fsnotify"
)

// watchDebounce coalesces the burst of create/write/rename events one atomic
// Save produces into a single reload signal.
const watchDebounce = 50 * time.Millisecond

// Notify watches both the config file and its parent directory. Neither
// alone is sufficient: atomic Save renames replace the inode and kill a
// file-only watch, while directory watches on some backends (kqueue) do not
// report in-place content writes at all. The file watch is re-armed after
// every create/rename/remove event naming the config file. Only events
// naming that file are forwarded; temporary files and the lock file in the
// same directory are ignored. Watcher errors (including queue overflows,
// where events may be lost) also produce a signal so a missed notification
// can never hide a change for longer than the debounce window.
func (s *Store) Notify(ctx context.Context) (<-chan struct{}, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf(
			"create YAML inference config watcher: %w",
			err,
		)
	}
	if err := watcher.Add(filepath.Dir(s.path)); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf(
			"watch YAML inference config directory: %w",
			err,
		)
	}
	// Best-effort: the file may not exist yet. Creation events re-arm this.
	_ = watcher.Add(s.path)
	signals := make(chan struct{}, 1)
	go s.forwardWatchEvents(ctx, watcher, signals)
	return signals, nil
}

func (s *Store) forwardWatchEvents(
	ctx context.Context,
	watcher *fsnotify.Watcher,
	signals chan<- struct{},
) {
	defer close(signals)
	defer func() { _ = watcher.Close() }()
	base := filepath.Base(s.path)
	var debounce *time.Timer
	var pending <-chan time.Time
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != base {
				continue
			}
			if event.Has(fsnotify.Create) ||
				event.Has(fsnotify.Rename) ||
				event.Has(fsnotify.Remove) {
				// Atomic replacement and deletion kill the in-place file
				// watch. Re-adding is harmless when the file is absent or
				// already watched, and retry happens on the next create.
				_ = watcher.Add(s.path)
			}
			if debounce == nil {
				debounce = time.NewTimer(watchDebounce)
			} else {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				debounce.Reset(watchDebounce)
			}
			pending = debounce.C
		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
			// A watch error can mean lost events: signal now rather than
			// risk hiding a change until the fallback poll.
			select {
			case signals <- struct{}{}:
			default:
			}
		case <-pending:
			pending = nil
			select {
			case signals <- struct{}{}:
			default:
			}
		}
	}
}

var _ config.Notifier = (*Store)(nil)
