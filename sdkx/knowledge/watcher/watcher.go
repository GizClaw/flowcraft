// Package watcher provides an fsnotify-backed knowledge.ChangeNotifier
// adapter.
//
// Pure-fs notification was deliberately split from the sdk core to keep
// sdk/knowledge dependency-free; reuse the SDK's Reloader to compose:
//
//	notifier, _ := watcher.New(store)
//	r := knowledge.NewReloader(store, notifier, knowledge.ReloaderOptions{})
//	go r.Run(ctx)
package watcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"github.com/fsnotify/fsnotify"
	otellog "go.opentelemetry.io/otel/log"
)

// Notifier wraps fsnotify.Watcher to satisfy knowledge.ChangeNotifier.
type Notifier struct {
	store     *knowledge.FSStore
	watcher   *fsnotify.Watcher
	out       chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	rootDir   string
}

// New constructs a Notifier for the given knowledge store. Returns nil
// without error when the underlying workspace doesn't expose a filesystem
// root (e.g. in-memory workspace).
func New(ctx context.Context, store *knowledge.FSStore) (*Notifier, error) {
	if store == nil {
		return nil, errors.New("knowledge/watcher: store is nil")
	}
	root := store.WorkspaceRoot()
	if root == "" {
		telemetry.Info(ctx, "knowledge: workspace does not support fsnotify watching")
		return nil, nil
	}
	knowledgeDir := filepath.Join(root, store.Prefix())
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
	if entries, err := os.ReadDir(knowledgeDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				_ = fw.Add(filepath.Join(knowledgeDir, entry.Name()))
			}
		}
	}
	n := &Notifier{
		store:   store,
		watcher: fw,
		out:     make(chan struct{}, 1),
		closed:  make(chan struct{}),
		rootDir: knowledgeDir,
	}
	n.wg.Add(1)
	go n.loop(ctx)
	telemetry.Info(ctx, "knowledge: watching for changes", otellog.String("dir", knowledgeDir))
	return n, nil
}

// Events implements knowledge.ChangeNotifier.
func (n *Notifier) Events() <-chan struct{} { return n.out }

// Close implements knowledge.ChangeNotifier.
func (n *Notifier) Close() error {
	n.closeOnce.Do(func() {
		close(n.closed)
		_ = n.watcher.Close()
	})
	n.wg.Wait()
	return nil
}

func (n *Notifier) loop(ctx context.Context) {
	defer n.wg.Done()
	defer close(n.out)
	for {
		select {
		case <-n.closed:
			return
		case <-ctx.Done():
			return
		case event, ok := <-n.watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) ||
				event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				// Watch newly-created subdirectories so future events are seen.
				if event.Has(fsnotify.Create) {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						_ = n.watcher.Add(event.Name)
					}
				}
				select {
				case n.out <- struct{}{}:
				default:
				}
			}
		case err, ok := <-n.watcher.Errors:
			if !ok {
				return
			}
			telemetry.Warn(ctx, "knowledge: watcher error", otellog.String("error", err.Error()))
		}
	}
}
