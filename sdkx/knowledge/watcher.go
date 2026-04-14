package knowledge

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

type rootedWorkspace interface {
	Root() string
}

type Watcher struct {
	ctx     context.Context
	store   *FSStore
	watcher *fsnotify.Watcher
	done    chan struct{}
	wg      sync.WaitGroup
}

func (s *FSStore) StartWatching(ctx context.Context) (*Watcher, error) {
	rw, ok := s.ws.(rootedWorkspace)
	if !ok {
		telemetry.Info(ctx, "knowledge: workspace does not support fsnotify watching")
		return nil, nil
	}

	knowledgeDir := filepath.Join(rw.Root(), s.prefix)
	if _, err := os.Stat(knowledgeDir); os.IsNotExist(err) {
		if err := os.MkdirAll(knowledgeDir, 0o755); err != nil {
			return nil, err
		}
	}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if err := fw.Add(knowledgeDir); err != nil {
		_ = fw.Close()
		return nil, err
	}

	entries, err := os.ReadDir(knowledgeDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				_ = fw.Add(filepath.Join(knowledgeDir, entry.Name()))
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

	telemetry.Info(ctx, "knowledge: watching for changes", otellog.String("dir", knowledgeDir))
	return w, nil
}

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
		ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
		defer cancel()

		if err := w.store.BuildIndex(ctx); err != nil {
			telemetry.Warn(ctx, "knowledge: hot reload failed", otellog.String("error", err.Error()))
		} else {
			telemetry.Info(ctx, "knowledge: hot reload completed")
		}

		rw, ok := w.store.ws.(rootedWorkspace)
		if !ok {
			return
		}
		knowledgeDir := filepath.Join(rw.Root(), w.store.prefix)
		entries, err := os.ReadDir(knowledgeDir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if entry.IsDir() {
				_ = w.watcher.Add(filepath.Join(knowledgeDir, entry.Name()))
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
			telemetry.Warn(w.ctx, "knowledge: watcher error", otellog.String("error", err.Error()))
		}
	}
}
