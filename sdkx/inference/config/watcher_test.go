package config

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/route"
)

type memoryStore struct {
	mu       sync.Mutex
	document Document
	revision string
	exists   bool
}

func (s *memoryStore) Load(context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.exists {
		return Snapshot{}, ErrNotFound
	}
	return Snapshot{Document: s.document.Clone(), Revision: s.revision}, nil
}

func (s *memoryStore) Save(
	_ context.Context,
	expectedRevision string,
	document Document,
) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expectedRevision != AnyRevision &&
		((expectedRevision == "" && s.exists) ||
			(expectedRevision != "" && expectedRevision != s.revision)) {
		return Snapshot{}, ErrConflict
	}
	if err := document.Validate(); err != nil {
		return Snapshot{}, err
	}
	s.revision += "."
	s.document = document.Clone()
	s.exists = true
	return Snapshot{Document: s.document.Clone(), Revision: s.revision}, nil
}

// notifyMemoryStore adds a controllable Notify to memoryStore.
type notifyMemoryStore struct {
	*memoryStore
	signals chan struct{}
	err     error
	closed  atomic.Bool
}

func (s *notifyMemoryStore) Notify(context.Context) (<-chan struct{}, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.signals, nil
}

func (s *notifyMemoryStore) signal() {
	s.signals <- struct{}{}
}

func (s *notifyMemoryStore) closeSignals() {
	if s.closed.CompareAndSwap(false, true) {
		close(s.signals)
	}
}

func waitForRevision(t *testing.T, reloader *Reloader, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if reloader.Snapshot().Revision == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("revision = %q, want %q", reloader.Snapshot().Revision, want)
}

func watcherDocument(name string) Document {
	return Document{
		Version: VersionV1,
		Providers: []ProviderConfig{{
			ID:     "fake",
			Driver: "fake",
			Spec:   []byte(`{"model":"` + name + `"}`),
		}},
	}
}

func newWatcherBuilder(t *testing.T) *Builder {
	t.Helper()
	builder, err := NewBuilder(
		map[string]Factory{
			"fake": FactoryFunc(func(
				_ context.Context,
				input ProviderInput,
			) (inference.ProviderDefinition, error) {
				return inference.ProviderDefinition{
					ID: input.ID,
					Models: []inference.ModelImplementation{{
						Descriptor: inference.ModelDescriptor{
							ID: inference.ModelID{Provider: input.ID, Name: "model"},
						},
						Openers: inference.Openers{
							Generate: func(
								context.Context,
								inference.ModelRef,
							) (inference.GenerateOperations, error) {
								return inference.GenerateOperations{}, nil
							},
						},
					}},
				}, nil
			}),
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	return builder
}

func TestReloaderSwapsRuntimeOnRevisionChange(t *testing.T) {
	store := &memoryStore{}
	if _, err := store.Save(
		t.Context(), "", watcherDocument("v1"),
	); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	reloader, err := NewReloader(t.Context(), newWatcherBuilder(t), store)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	first := reloader.Runtime()
	firstRevision := reloader.Snapshot().Revision

	// Unchanged revision is a no-op and keeps the same runtime pointer.
	if err := reloader.ReloadOnce(t.Context()); err != nil {
		t.Fatalf("ReloadOnce unchanged: %v", err)
	}
	if reloader.Runtime() != first {
		t.Fatal("unchanged revision swapped the runtime")
	}

	if _, err := store.Save(
		t.Context(), firstRevision, watcherDocument("v2"),
	); err != nil {
		t.Fatalf("update Save: %v", err)
	}
	if err := reloader.ReloadOnce(t.Context()); err != nil {
		t.Fatalf("ReloadOnce changed: %v", err)
	}
	if reloader.Runtime() == first {
		t.Fatal("changed revision kept the old runtime")
	}
	if reloader.Snapshot().Revision == firstRevision {
		t.Fatal("snapshot revision did not advance")
	}
}

func TestReloaderKeepsLastGoodRuntimeOnBuildFailure(t *testing.T) {
	store := &memoryStore{}
	if _, err := store.Save(
		t.Context(), "", watcherDocument("v1"),
	); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	reloader, err := NewReloader(t.Context(), newWatcherBuilder(t), store)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	healthy := reloader.Runtime()

	// A document that fails validation inside the factory-less builder path:
	// unknown driver still produces an error at build time.
	broken := Document{
		Version: VersionV1,
		Providers: []ProviderConfig{{
			ID: "fake", Driver: "missing",
		}},
	}
	if _, err := store.Save(t.Context(), AnyRevision, broken); err != nil {
		t.Fatalf("broken Save: %v", err)
	}
	if err := reloader.ReloadOnce(t.Context()); err == nil {
		t.Fatal("ReloadOnce succeeded with an unbuildable document")
	}
	if reloader.Runtime() != healthy {
		t.Fatal("failed reload swapped the runtime")
	}
}

func TestReloaderRunReportsFailuresAndStopsOnCancel(t *testing.T) {
	store := &memoryStore{}
	if _, err := store.Save(
		t.Context(), "", watcherDocument("v1"),
	); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	reloader, err := NewReloader(t.Context(), newWatcherBuilder(t), store)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	if _, err := store.Save(t.Context(), AnyRevision, Document{
		Version:   VersionV1,
		Providers: []ProviderConfig{{ID: "fake", Driver: "missing"}},
	}); err != nil {
		t.Fatalf("broken Save: %v", err)
	}
	var failures atomic.Int64
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- reloader.Run(ctx, 10*time.Millisecond, func(error) {
			if failures.Add(1) == 2 {
				cancel()
			}
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if failures.Load() < 1 {
		t.Fatal("onError never observed the build failure")
	}
}

func TestNewReloaderRejectsBrokenInitialSnapshot(t *testing.T) {
	store := &memoryStore{}
	if _, err := NewReloader(t.Context(), newWatcherBuilder(t), store); !errors.Is(err, ErrNotFound) {
		t.Fatalf("NewReloader error = %v, want ErrNotFound", err)
	}
}

func watcherRoutedDocument(name string) Document {
	document := watcherDocument(name)
	document.Route = &route.Policy{
		Generate: []route.Pool{{
			Tier: "balanced",
			Targets: []route.Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "fake", Name: "model"},
				},
			}},
		}},
	}
	return document
}

func TestReloaderSwapsRouterWithRuntime(t *testing.T) {
	store := &memoryStore{}
	seed, err := store.Save(t.Context(), "", watcherRoutedDocument("v1"))
	if err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	reloader, err := NewReloader(t.Context(), newWatcherBuilder(t), store)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	firstRuntime := reloader.Runtime()
	firstRouter := reloader.Router()
	if firstRouter == nil {
		t.Fatal("route section did not produce a router")
	}

	if err := reloader.ReloadOnce(t.Context()); err != nil {
		t.Fatalf("ReloadOnce unchanged: %v", err)
	}
	if reloader.Router() != firstRouter {
		t.Fatal("unchanged revision swapped the router")
	}

	if _, err := store.Save(
		t.Context(), seed.Revision, watcherRoutedDocument("v2"),
	); err != nil {
		t.Fatalf("update Save: %v", err)
	}
	if err := reloader.ReloadOnce(t.Context()); err != nil {
		t.Fatalf("ReloadOnce changed: %v", err)
	}
	if reloader.Runtime() == firstRuntime {
		t.Fatal("changed revision kept the old runtime")
	}
	if reloader.Router() == firstRouter {
		t.Fatal("changed revision kept the old router")
	}
}

func TestReloaderRouterStaysNilWithoutRouteSection(t *testing.T) {
	store := &memoryStore{}
	if _, err := store.Save(t.Context(), "", watcherDocument("v1")); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	reloader, err := NewReloader(t.Context(), newWatcherBuilder(t), store)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	if reloader.Router() != nil {
		t.Fatal("routeless document produced a router")
	}
	if _, err := store.Save(t.Context(), AnyRevision, watcherDocument("v2")); err != nil {
		t.Fatalf("update Save: %v", err)
	}
	if err := reloader.ReloadOnce(t.Context()); err != nil {
		t.Fatalf("ReloadOnce: %v", err)
	}
	if reloader.Router() != nil {
		t.Fatal("router appeared without a route section")
	}
}

func TestReloaderWatchReloadsOnSignal(t *testing.T) {
	mem := &memoryStore{}
	seed, err := mem.Save(t.Context(), "", watcherDocument("v1"))
	if err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	store := &notifyMemoryStore{memoryStore: mem, signals: make(chan struct{}, 1)}
	reloader, err := NewReloader(t.Context(), newWatcherBuilder(t), store)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// No ticker fallback: one hour proves every reload below came from
		// the signal path.
		done <- reloader.Watch(ctx, time.Hour, func(err error) {
			t.Errorf("onError: %v", err)
		})
	}()

	updated, err := mem.Save(t.Context(), seed.Revision, watcherDocument("v2"))
	if err != nil {
		t.Fatalf("update Save: %v", err)
	}
	store.signal()
	waitForRevision(t, reloader, updated.Revision)

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch error = %v, want context.Canceled", err)
	}
}

func TestReloaderWatchFallsBackWhenWatchDies(t *testing.T) {
	mem := &memoryStore{}
	if _, err := mem.Save(t.Context(), "", watcherDocument("v1")); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	store := &notifyMemoryStore{memoryStore: mem, signals: make(chan struct{}, 1)}
	reloader, err := NewReloader(t.Context(), newWatcherBuilder(t), store)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	var failures atomic.Int64
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- reloader.Watch(ctx, 10*time.Millisecond, func(error) {
			failures.Add(1)
		})
	}()
	// Killing the signal channel must not kill the loop: the fallback
	// ticker takes over.
	store.closeSignals()
	updated, err := mem.Save(t.Context(), AnyRevision, watcherDocument("v2"))
	if err != nil {
		t.Fatalf("update Save: %v", err)
	}
	waitForRevision(t, reloader, updated.Revision)
	if failures.Load() == 0 {
		t.Fatal("closed watch channel was never reported")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch error = %v, want context.Canceled", err)
	}
}

func TestReloaderWatchPollsWhenStoreCannotNotify(t *testing.T) {
	t.Run("no notifier interface", func(t *testing.T) {
		mem := &memoryStore{}
		if _, err := mem.Save(t.Context(), "", watcherDocument("v1")); err != nil {
			t.Fatalf("seed Save: %v", err)
		}
		reloader, err := NewReloader(t.Context(), newWatcherBuilder(t), mem)
		if err != nil {
			t.Fatalf("NewReloader: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- reloader.Watch(ctx, 10*time.Millisecond, func(err error) {
				t.Errorf("onError: %v", err)
			})
		}()
		updated, err := mem.Save(t.Context(), AnyRevision, watcherDocument("v2"))
		if err != nil {
			t.Fatalf("update Save: %v", err)
		}
		waitForRevision(t, reloader, updated.Revision)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch error = %v, want context.Canceled", err)
		}
	})

	t.Run("unsupported notification", func(t *testing.T) {
		mem := &memoryStore{}
		if _, err := mem.Save(t.Context(), "", watcherDocument("v1")); err != nil {
			t.Fatalf("seed Save: %v", err)
		}
		store := &notifyMemoryStore{
			memoryStore: mem,
			signals:     make(chan struct{}, 1),
			err:         ErrNotifyUnsupported,
		}
		reloader, err := NewReloader(t.Context(), newWatcherBuilder(t), store)
		if err != nil {
			t.Fatalf("NewReloader: %v", err)
		}
		var failures atomic.Int64
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- reloader.Watch(ctx, 10*time.Millisecond, func(error) {
				failures.Add(1)
			})
		}()
		updated, err := mem.Save(t.Context(), AnyRevision, watcherDocument("v2"))
		if err != nil {
			t.Fatalf("update Save: %v", err)
		}
		waitForRevision(t, reloader, updated.Revision)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch error = %v, want context.Canceled", err)
		}
		// ErrNotifyUnsupported is a silent capability fallback, not an error.
		if failures.Load() != 0 {
			t.Fatalf("unsupported notifier reported %d errors", failures.Load())
		}
	})
}
