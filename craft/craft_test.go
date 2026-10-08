package craft

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime"
)

type testCapability struct{}

func (testCapability) Name() string { return "test" }

func (testCapability) Register(registry *resource.Registry) error {
	if err := event.Register(registry); err != nil {
		return err
	}
	return registry.Register(runtimeContextFactory{})
}

type runtimeContextFactory struct{}

func (runtimeContextFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.RuntimeContext",
		Deps: []resource.DepSpec{{
			Name: "runtime", Type: RuntimeContextContract, Required: true,
		}},
	}
}

func (runtimeContextFactory) New(
	_ context.Context,
	in resource.Input,
) (any, error) {
	value, _ := in.Dep("runtime")
	return value, nil
}

const testDeploy = `
version: v1
resources:
  bus:
    kind: event.Bus
    impl: memory
  context:
    kind: test.RuntimeContext
    deps:
      runtime: craft.runtime
runtime:
  event_bus: bus
`

func TestCraftLifecycleInlineDeploy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	def, err := ParseDefinition([]byte(`
craft:
  id: test
  name: Test
  version: 0.1.0
deploy:
` + indent(testDeploy, "  ")))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	c, err := New(def, Options{
		ConfigDir:    dir,
		DataDir:      dir,
		Capabilities: []Capability{testCapability{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assertLifecycle(t, c)
}

func TestCraftLifecycleBaseLayers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "base.yaml")
	if err := os.WriteFile(path, []byte(testDeploy), 0o600); err != nil {
		t.Fatalf("write base layer: %v", err)
	}

	def, err := ParseDefinition([]byte(`
craft:
  id: test
  name: Test
  version: 0.1.0
base_layers:
  - name: base
    file: base.yaml
`))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	c, err := New(def, Options{
		ConfigDir:     dir,
		DataDir:       dir,
		DefinitionDir: dir,
		Capabilities:  []Capability{testCapability{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assertLifecycle(t, c)
}

func TestCraftLifecycleEmbeddedLayers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	assets := fstest.MapFS{
		"base.yaml": &fstest.MapFile{Data: []byte(testDeploy)},
	}
	def, err := ParseDefinition([]byte(`
craft:
  id: test
  name: Test
  version: 0.1.0
base_layers:
  - name: base
    embed: base.yaml
`))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	c, err := New(def, Options{
		ConfigDir:    dir,
		DataDir:      dir,
		Assets:       assets,
		Capabilities: []Capability{testCapability{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assertLifecycle(t, c)
}

func assertLifecycle(t *testing.T, c *Craft) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{}); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("OpenRuntime before Start = %v, want ErrNotStarted", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start (idempotent): %v", err)
	}
	if _, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{
		Values: map[string]string{"WORKSPACE": "/ws"},
	}); err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	rt, ok := c.Runtime(DefaultKey)
	if !ok {
		t.Fatal("Runtime not registered")
	}
	value, ok := rt.Resource("context")
	if !ok {
		t.Fatal("context resource missing")
	}
	contextValue, ok := value.(RuntimeContext)
	if !ok {
		t.Fatalf("context resource = %T, want RuntimeContext", value)
	}
	if contextValue.Key != DefaultKey {
		t.Fatalf("RuntimeContext.Key = %q, want %q", contextValue.Key, DefaultKey)
	}
	if got := contextValue.Values["WORKSPACE"]; got != "/ws" {
		t.Fatalf("RuntimeContext.Values[WORKSPACE] = %q, want /ws", got)
	}
	if _, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{}); !errors.Is(err, ErrRuntimeExists) {
		t.Fatalf("duplicate OpenRuntime error = %v, want ErrRuntimeExists", err)
	}
	if err := c.ReloadRuntime(ctx, DefaultKey, ReasonManual); err != nil {
		t.Fatalf("ReloadRuntime: %v", err)
	}
	if err := c.ReloadAll(ctx, ReasonConfig); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	if err := c.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := c.CloseRuntime(ctx, DefaultKey); err != nil {
		t.Fatalf("CloseRuntime: %v", err)
	}
	if _, ok := c.Runtime(DefaultKey); ok {
		t.Fatal("runtime still registered after CloseRuntime")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close (idempotent): %v", err)
	}
	if err := c.Start(ctx); !errors.Is(err, ErrCraftClosed) {
		t.Fatalf("Start after Close = %v, want ErrCraftClosed", err)
	}
}

type binderCapability struct {
	mu    sync.Mutex
	calls []RuntimeKey
}

func (*binderCapability) Name() string { return "binder" }

func (*binderCapability) Register(*resource.Registry) error { return nil }

func (b *binderCapability) BindRuntime(
	_ context.Context,
	key RuntimeKey,
	_ *runtime.Runtime,
) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, key)
	return nil
}

func (b *binderCapability) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

type externalCapability struct{}

func (externalCapability) Name() string { return "external" }

func (externalCapability) Register(registry *resource.Registry) error {
	if err := event.Register(registry); err != nil {
		return err
	}
	return registry.Register(externalFactory{})
}

type externalFactory struct{}

func (externalFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.External",
		Deps: []resource.DepSpec{{
			Name: "value", Type: "test.Value", Required: true,
		}},
	}
}

func (externalFactory) New(
	_ context.Context,
	in resource.Input,
) (any, error) {
	value, _ := in.Dep("value")
	return value, nil
}

func TestCraftExternalsAndRuntimeBinder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	def, err := ParseDefinition([]byte(`
craft:
  id: test
  name: Test
  version: 0.1.0
deploy:
  version: v1
  resources:
    bus:
      kind: event.Bus
      impl: memory
    ext:
      kind: test.External
      deps:
        value: app.value
  runtime:
    event_bus: bus
`))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	binder := &binderCapability{}
	c, err := New(def, Options{
		ConfigDir:    dir,
		DataDir:      dir,
		Capabilities: []Capability{externalCapability{}, binder},
		Externals: []deploy.ExternalResource{{
			External: deploy.External{Name: "app.value", Contract: "test.Value"},
			Value:    "hello",
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rt, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{})
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	value, ok := rt.Resource("ext")
	if !ok || value != "hello" {
		t.Fatalf("external resource = %#v, want hello", value)
	}
	if got := binder.count(); got != 1 {
		t.Fatalf("binder calls after OpenRuntime = %d, want 1", got)
	}
	if err := c.ReloadRuntime(ctx, DefaultKey, ReasonManual); err != nil {
		t.Fatalf("ReloadRuntime: %v", err)
	}
	if got := binder.count(); got != 2 {
		t.Fatalf("binder calls after ReloadRuntime = %d, want 2", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// indent prefixes every non-empty line.
func indent(text, prefix string) string {
	var out []byte
	lineStart := true
	for i := 0; i < len(text); i++ {
		if lineStart && text[i] != '\n' {
			out = append(out, prefix...)
		}
		out = append(out, text[i])
		lineStart = text[i] == '\n'
	}
	return string(out)
}
