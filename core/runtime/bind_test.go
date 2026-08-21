package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

const (
	testBinderKind = "runtime.SessionManagerBinder"
	testBinderImpl = "test"
)

// freshBusFactory builds a new bus per generation so reloads satisfy the
// "each generation owns its bus" constraint.
type freshBusFactory struct{}

func (freshBusFactory) Spec() resource.Spec {
	return resource.Spec{Kind: testEventKind, Impl: testEventImpl}
}

func (freshBusFactory) New(context.Context, resource.Input) (any, error) {
	return event.NewMemoryBus(), nil
}

// newBindRegistry registers the test engine, a per-generation bus, a
// shared checkpoint store, and the given extra factories.
func newBindRegistry(t *testing.T, extras ...resource.Factory) *resource.Registry {
	t.Helper()
	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: noopEngine()})
	reg.MustRegister(freshBusFactory{})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: testStoreKind, Impl: testStoreImpl},
		value: &recordingCheckpointStore{},
	})
	for _, factory := range extras {
		if factory != nil {
			reg.MustRegister(factory)
		}
	}
	return reg
}

type recordingBinder struct {
	mu      sync.Mutex
	manager *session.Manager
}

func (b *recordingBinder) BindSessionManager(manager *session.Manager) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.manager != nil {
		return errdefs.Conflictf("recording binder: already bound")
	}
	b.manager = manager
	return nil
}

func (b *recordingBinder) bound() *session.Manager {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.manager
}

type binderFactory struct {
	mu      sync.Mutex
	created []*recordingBinder
}

func (f *binderFactory) Spec() resource.Spec {
	return resource.Spec{Kind: testBinderKind, Impl: testBinderImpl}
}

func (f *binderFactory) New(context.Context, resource.Input) (any, error) {
	binder := &recordingBinder{}
	f.mu.Lock()
	f.created = append(f.created, binder)
	f.mu.Unlock()
	return binder, nil
}

func (f *binderFactory) all() []*recordingBinder {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*recordingBinder(nil), f.created...)
}

func binderDoc(t *testing.T, extra string) deploy.Document {
	t.Helper()
	doc := baseRuntimeDoc(t)
	doc.Resources["binder"] = resource.Resource{
		Kind: testBinderKind, Impl: testBinderImpl,
	}
	if extra != "" {
		doc.Resources["binder2"] = resource.Resource{
			Kind: testBinderKind, Impl: testBinderImpl,
		}
	}
	return doc
}

func TestBuildBindsSessionManager(t *testing.T) {
	factory := &binderFactory{}
	reg := newBindRegistry(t, factory)
	app, err := NewBuilder(reg).Build(context.Background(), binderDoc(t, ""))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	created := factory.all()
	if len(created) != 1 {
		t.Fatalf("created binders = %d, want 1", len(created))
	}
	if got := created[0].bound(); got != app.Sessions() {
		t.Fatal("binder did not receive the runtime session manager")
	}
}

func TestBuildRejectsMultipleBinders(t *testing.T) {
	factory := &binderFactory{}
	reg := newBindRegistry(t, factory)
	if _, err := NewBuilder(reg).Build(context.Background(), binderDoc(t, "extra")); !errdefs.IsConflict(err) {
		t.Fatalf("Build with two binders = %v, want conflict", err)
	}
}

func TestReloadRebindsNewGeneration(t *testing.T) {
	factory := &binderFactory{}
	reg := newBindRegistry(t, factory)
	doc := binderDoc(t, "")
	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	if _, err := app.Reload(context.Background(), doc); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	created := factory.all()
	if len(created) != 2 {
		t.Fatalf("created binders = %d, want 2 (one per generation)", len(created))
	}
	for _, binder := range created {
		if got := binder.bound(); got != app.Sessions() {
			t.Fatal("every generation's binder must receive the same runtime manager")
		}
	}
}

func TestReloadWithoutBinderDoesNotBind(t *testing.T) {
	factory := &binderFactory{}
	reg := newBindRegistry(t, factory)
	doc := binderDoc(t, "")
	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	withoutBinder := baseRuntimeDoc(t)
	if _, err := app.Reload(context.Background(), withoutBinder); err != nil {
		t.Fatalf("Reload without binder: %v", err)
	}
	if got := len(factory.all()); got != 1 {
		t.Fatalf("created binders = %d, want 1 (new generation has no binder)", got)
	}
}

func TestBuildDelegationServiceSessionPath(t *testing.T) {
	reg := newBaseRegistry(t, event.NewMemoryBus(), &recordingCheckpointStore{}, noopEngine())
	reg.MustRegister(delegation.NewDirectoryFactory())
	reg.MustRegister(delegation.NewServiceFactory())
	doc := baseRuntimeDoc(t)
	doc.Resources["delegation_directory"] = resource.Resource{
		Kind: delegation.DirectoryKind, Impl: "local",
	}
	doc.Resources["delegation"] = resource.Resource{
		Kind: delegation.ServiceKind, Impl: "local",
		Deps: resource.Deps{"directory": "delegation_directory"},
	}
	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	value, _ := app.Resource("delegation")
	service, ok := value.(*delegation.LocalService)
	if !ok {
		t.Fatalf("delegation resource = %T, want *delegation.LocalService",
			value)
	}
	response, err := service.Delegate(context.Background(), delegation.Request{
		Mode:   delegation.ModeSync,
		Target: "bot",
		Input:  "do it",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != delegation.StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	if response.Metadata["delegation.session_id"] == "" {
		t.Fatal("session-path delegation returned no delegation.session_id")
	}
}
