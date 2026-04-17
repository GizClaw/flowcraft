package skill

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"github.com/fsnotify/fsnotify"
	otellog "go.opentelemetry.io/otel/log"
)

// RootedWorkspace is implemented by workspace types that expose a root path.
type RootedWorkspace interface {
	Root() string
}

// Watcher monitors the skills directory for filesystem changes and triggers
// automatic re-indexing. Uses fsnotify for efficient event-driven watching.
type Watcher struct {
	ctx     context.Context
	store   *SkillStore
	watcher *fsnotify.Watcher
	done    chan struct{}
	wg      sync.WaitGroup
}

// StartWatching creates and starts a filesystem watcher on the skills directory.
// Only works when the underlying Workspace has a Root() method (LocalWorkspace).
// Returns nil without error if the workspace does not support watching.
func (s *SkillStore) StartWatching(ctx context.Context) (*Watcher, error) {
	rw, ok := s.ws.(RootedWorkspace)
	if !ok {
		telemetry.Info(ctx, "skill: workspace does not support fsnotify watching")
		return nil, nil
	}

	skillsDir := filepath.Join(rw.Root(), s.prefix)
	if _, err := os.Stat(skillsDir); os.IsNotExist(err) {
		if err := os.MkdirAll(skillsDir, 0o755); err != nil {
			return nil, err
		}
	}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	// Watch the skills root directory
	if err := fw.Add(skillsDir); err != nil {
		_ = fw.Close()
		return nil, err
	}

	// Also watch each subdirectory for SKILL.md changes
	entries, err := os.ReadDir(skillsDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				subDir := filepath.Join(skillsDir, entry.Name())
				_ = fw.Add(subDir)
			}
		}
	}

	w := &Watcher{
		ctx:     ctx,
		store:   s,
		watcher: fw,
		done:    make(chan struct{}),
	}
	w.wg.Add(1)
	go w.loop()

	telemetry.Info(ctx, "skill: watching for changes", otellog.String("dir", skillsDir))
	return w, nil
}

// Stop closes the watcher and waits for the loop to exit.
func (w *Watcher) Stop() {
	close(w.done)
	_ = w.watcher.Close()
	w.wg.Wait()
}

func (w *Watcher) loop() {
	defer w.wg.Done()

	var debounceTimer *time.Timer
	rebuildPending := false
	rebuildCh := make(chan struct{}, 1)

	rebuild := func() {
		ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
		defer cancel()

		if err := w.store.BuildIndex(ctx); err != nil {
			telemetry.Warn(ctx, "skill: hot reload failed", otellog.String("error", err.Error()))
		} else {
			telemetry.Info(ctx, "skill: hot reload completed")
		}

		rw, ok := w.store.ws.(RootedWorkspace)
		if !ok {
			return
		}
		skillsDir := filepath.Join(rw.Root(), w.store.prefix)
		entries, err := os.ReadDir(skillsDir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if entry.IsDir() {
				_ = w.watcher.Add(filepath.Join(skillsDir, entry.Name()))
			}
		}
	}

	for {
		select {
		case <-w.done:
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return

		case <-rebuildCh:
			rebuild()
			rebuildPending = false

		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) ||
				event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				if !rebuildPending {
					rebuildPending = true
					if debounceTimer != nil {
						debounceTimer.Stop()
					}
					debounceTimer = time.AfterFunc(500*time.Millisecond, func() {
						select {
						case rebuildCh <- struct{}{}:
						default:
						}
					})
				}
			}

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			telemetry.Warn(w.ctx, "skill: watcher error", otellog.String("error", err.Error()))
		}
	}
}
