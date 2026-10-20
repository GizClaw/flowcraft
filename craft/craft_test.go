package craft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

type testCapability struct{}

func (testCapability) Name() string { return "test" }

func (testCapability) Register(registry *resource.Registry) error {
	if err := event.Register(registry); err != nil {
		return err
	}
	return resource.Register(registry, runtimeContextFactory{})
}

type runtimeContextFactory struct{}

func (runtimeContextFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.RuntimeContext",
		Deps: []resource.Requirement{resource.Require[RuntimeContext]("runtime").Spec()},
	}
}

func (runtimeContextFactory) New(
	_ context.Context,
	in resource.Input,
) (any, error) {
	value, _, _ := resource.Require[RuntimeContext]("runtime").TryGet(in)
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

// TestCraftLifecycleBaseLayersInWorkingDirectory pins the documented
// default of Options.DefinitionDir: empty means the process working
// directory, so a file base layer beside it loads without the caller
// naming an anchor.
func TestCraftLifecycleBaseLayersInWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "base.yaml"), []byte(testDeploy), 0o600,
	); err != nil {
		t.Fatalf("write base layer: %v", err)
	}
	t.Chdir(dir)
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
		ConfigDir:    dir,
		DataDir:      dir,
		Capabilities: []Capability{testCapability{}},
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
	// failAt makes the call with this ordinal (counting from 1) fail, so
	// a test can break the bind of one reload.
	failAt  int
	failErr error
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
	if b.failAt > 0 && len(b.calls) == b.failAt {
		return b.failErr
	}
	return nil
}

func (b *binderCapability) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// parkingBinder holds the bind of one call until the test releases it, so
// a test can interleave a close with a reload that has already swapped
// generations but not finished binding.
type parkingBinder struct {
	parkAt  int
	entered chan struct{}
	release chan struct{}
	// failErr is what the parked call reports once the test releases it:
	// the bind that was in flight while something else took the key.
	failErr error

	mu    sync.Mutex
	calls int
}

func (*parkingBinder) Name() string { return "parking" }

func (*parkingBinder) Register(*resource.Registry) error { return nil }

func (b *parkingBinder) BindRuntime(
	_ context.Context,
	_ RuntimeKey,
	_ *runtime.Runtime,
) error {
	b.mu.Lock()
	b.calls++
	park := b.calls == b.parkAt
	b.mu.Unlock()
	if !park {
		return nil
	}
	close(b.entered)
	<-b.release
	return b.failErr
}

type externalCapability struct{}

func (externalCapability) Name() string { return "external" }

func (externalCapability) Register(registry *resource.Registry) error {
	if err := event.Register(registry); err != nil {
		return err
	}
	return resource.Register(registry, externalFactory{})
}

type externalFactory struct{}

func (externalFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.External",
		Deps: []resource.Requirement{resource.Require[any]("value").Spec()},
	}
}

func (externalFactory) New(
	_ context.Context,
	in resource.Input,
) (any, error) {
	value, _, _ := resource.Require[any]("value").TryGet(in)
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
		Externals:    []deploy.ExternalResource{deploy.NewExternal("app.value", "test.Value", "hello")},
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

// TestReloadRuntimeWithReplacesRuntimeOptions covers the reload that
// carries new runtime inputs: the generation is composed with the
// options the caller passed, and the key keeps them afterwards — a later
// plain reload or ReloadAll recomposes with them instead of the ones
// OpenRuntime stored.
func TestReloadRuntimeWithReplacesRuntimeOptions(t *testing.T) {
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
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Two overlays that are told apart by the resource they declare.
	writeOverlay := func(name, kind string) RuntimeOptions {
		t.Helper()
		file := name + ".yaml"
		content := "resources:\n  " + kind + ":\n" +
			"    kind: test.RuntimeContext\n" +
			"    deps:\n      runtime: craft.runtime\n"
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
			t.Fatalf("write overlay %s: %v", file, err)
		}
		return RuntimeOptions{Layers: []deploy.Layer{{
			Name:     name,
			Priority: 30,
			Source:   resource.Source{File: file},
			BaseDir:  dir,
		}}}
	}
	first := writeOverlay("first", "gen1")
	second := writeOverlay("second", "gen2")
	rt, err := c.OpenRuntime(ctx, DefaultKey, first)
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	has := func(kind string) bool {
		t.Helper()
		_, ok := rt.Resource(kind)
		return ok
	}
	if !has("gen1") || has("gen2") {
		t.Fatal("the generation opened with the first overlay is not the one serving")
	}

	result, err := c.ReloadRuntimeWith(ctx, DefaultKey, second, ReasonManual)
	if err != nil {
		t.Fatalf("ReloadRuntimeWith: %v", err)
	}
	// The runtime was opened one generation ago and core numbers the
	// first one 1: this reload installs 2 and replaces 1.
	if result == nil || result.GenerationID != 2 || result.PreviousID != 1 {
		t.Fatalf("ReloadRuntimeWith result = %+v, want generation 2 replacing 1",
			result)
	}
	if !has("gen2") || has("gen1") {
		t.Fatal("ReloadRuntimeWith did not compose the generation with the options it was given")
	}

	// The key now holds the replacement options: a plain reload must
	// recompose from the second overlay, not fall back to the first.
	if err := c.ReloadRuntime(ctx, DefaultKey, ReasonManual); err != nil {
		t.Fatalf("ReloadRuntime: %v", err)
	}
	if !has("gen2") || has("gen1") {
		t.Fatal("ReloadRuntime reused the options the runtime was opened with")
	}
	if err := c.ReloadAll(ctx, ReasonConfig); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	if !has("gen2") || has("gen1") {
		t.Fatal("ReloadAll reused the options the runtime was opened with")
	}

	if _, err := c.ReloadRuntimeWith(ctx, "missing", RuntimeOptions{},
		ReasonManual); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("ReloadRuntimeWith(missing) = %v, want ErrRuntimeNotFound", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestReloadRuntimeRollsBackWhenBindingFails covers the half of a reload
// that happens after the runtime has already swapped generations: a
// binder that fails there has to leave the caller with the generation it
// had, not with a new one nothing is bound to.
func TestReloadRuntimeRollsBackWhenBindingFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	writeBase := func(extra string) {
		t.Helper()
		content := `
version: v1
resources:
  bus:
    kind: event.Bus
    impl: memory
  context:
    kind: test.RuntimeContext
    deps:
      runtime: craft.runtime
` + extra + `
runtime:
  event_bus: bus
`
		if err := os.WriteFile(base, []byte(content), 0o600); err != nil {
			t.Fatalf("write base layer: %v", err)
		}
	}
	writeBase("")
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
	binder := &binderCapability{failAt: 2, failErr: errors.New("registry is locked")}
	c, err := New(def, Options{
		ConfigDir:     dir,
		DataDir:       dir,
		DefinitionDir: dir,
		Capabilities:  []Capability{testCapability{}, binder},
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
	if _, ok := rt.Resource("context"); !ok {
		t.Fatal("the first generation is missing its resource")
	}

	// The next generation adds a resource. The reload swaps it in and
	// then fails to bind it, so the rollback has to bring the previous
	// generation back.
	writeBase(`
  next:
    kind: test.RuntimeContext
    deps:
      runtime: craft.runtime
`)
	reloadErr := c.ReloadRuntime(ctx, DefaultKey, ReasonManual)
	if reloadErr == nil {
		t.Fatal("ReloadRuntime bound a generation its binder refused")
	}
	if !strings.Contains(reloadErr.Error(), "rolled back") {
		t.Fatalf("ReloadRuntime = %v, want it to report the rollback", reloadErr)
	}
	if got := binder.count(); got != 3 {
		t.Fatalf("binder calls = %d, want the open, the failed reload and the rebind",
			got)
	}
	if _, ok := rt.Resource("next"); ok {
		t.Fatal("the generation whose bind failed is still serving")
	}
	if _, ok := rt.Resource("context"); !ok {
		t.Fatal("the previous generation is not serving after the rollback")
	}

	// The rollback re-installs the previous document under a fresh
	// generation id — core allocates one per attempt, so the failed call
	// and its rollback each consumed one — and the error names the id
	// that ends up serving, because the caller's own id no longer
	// describes it. The next reload is the proof: it replaces exactly
	// the generation the error named.
	result, err := c.ReloadRuntimeWith(ctx, DefaultKey, RuntimeOptions{},
		ReasonManual)
	if err != nil {
		t.Fatalf("ReloadRuntimeWith after the rollback: %v", err)
	}
	if result == nil {
		t.Fatal("ReloadRuntimeWith after the rollback returned no generation")
	}
	if want := fmt.Sprintf("generation %d", result.PreviousID); !strings.Contains(
		reloadErr.Error(), want) {
		t.Fatalf("ReloadRuntime = %v, want it to name %s, the generation "+
			"serving after the rollback", reloadErr, want)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// reloadProbeCapability registers the external-consuming factory the
// reload tests observe an injected value through.
type reloadProbeCapability struct{}

func (reloadProbeCapability) Name() string { return "reloadprobe" }

func (reloadProbeCapability) Register(registry *resource.Registry) error {
	return resource.Register(registry, reloadProbeFactory{})
}

// reloadProbeFactory reports both halves of the per-runtime inputs one
// generation was built with: the injected external it depends on and the
// ${craft:name} value its settings were expanded with.
type reloadProbeFactory struct{}

func (reloadProbeFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.ReloadProbe",
		Deps: []resource.Requirement{resource.Require[any]("db").Spec()},
	}
}

func (reloadProbeFactory) New(_ context.Context, in resource.Input) (any, error) {
	var settings struct {
		Name string `json:"name"`
	}
	if len(in.Settings) > 0 {
		if err := json.Unmarshal(in.Settings, &settings); err != nil {
			return nil, err
		}
	}
	value, _, _ := resource.Require[any]("db").TryGet(in)
	return reloadProbe{Value: value, Name: settings.Name}, nil
}

// reloadProbe is what one reloadProbeFactory instance saw.
type reloadProbe struct {
	Value any
	Name  string
}

// reloadTestCraft starts a Craft with the given capabilities and returns
// it with a closed-over context.
func reloadTestCraft(
	t *testing.T,
	dir string,
	capabilities ...Capability,
) (*Craft, context.Context) {
	t.Helper()
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
		Capabilities: capabilities,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return c, ctx
}

// TestReloadRuntimeWithReplacesRuntimeInputs covers the per-runtime
// inputs a reload carries beyond the layers: ${craft:...} values (both
// the injected craft.runtime external and the resolver the document is
// expanded with) and injected externals are the ones the new options
// describe, and a later plain reload or ReloadAll recomposes with them.
func TestReloadRuntimeWithReplacesRuntimeInputs(t *testing.T) {
	dir := t.TempDir()
	c, ctx := reloadTestCraft(t, dir, testCapability{}, reloadProbeCapability{})
	overlay := "resources:\n  probe:\n    kind: test.ReloadProbe\n" +
		"    settings:\n      name: \"${craft:name}\"\n" +
		"    deps:\n      db: db\n"
	if err := os.WriteFile(filepath.Join(dir, "overlay.yaml"),
		[]byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	layers := []deploy.Layer{{
		Name:     "overlay",
		Priority: 30,
		Source:   resource.Source{File: "overlay.yaml"},
		BaseDir:  dir,
	}}
	options := func(name, db string) RuntimeOptions {
		return RuntimeOptions{
			Layers:    layers,
			Values:    map[string]string{"name": name},
			Externals: []deploy.ExternalResource{deploy.NewExternal("db", "db.Pool", db)},
		}
	}
	rt, err := c.OpenRuntime(ctx, DefaultKey, options("gen1", "one"))
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	probe := func() reloadProbe {
		t.Helper()
		value, ok := rt.Resource("probe")
		if !ok {
			t.Fatal("probe resource missing")
		}
		got, ok := value.(reloadProbe)
		if !ok {
			t.Fatalf("probe resource is %T", value)
		}
		return got
	}
	if got := probe(); got.Value != "one" || got.Name != "gen1" {
		t.Fatalf("generation 1 probe = %+v, want value one, name gen1", got)
	}

	if _, err := c.ReloadRuntimeWith(ctx, DefaultKey, options("gen2", "two"),
		ReasonManual); err != nil {
		t.Fatalf("ReloadRuntimeWith: %v", err)
	}
	if got := probe(); got.Value != "two" || got.Name != "gen2" {
		t.Fatalf("reloaded probe = %+v, want value two, name gen2", got)
	}

	// The key keeps the replacement inputs: both a plain reload and
	// ReloadAll rebuild the generation with them.
	if err := c.ReloadRuntime(ctx, DefaultKey, ReasonManual); err != nil {
		t.Fatalf("ReloadRuntime: %v", err)
	}
	if got := probe(); got.Value != "two" || got.Name != "gen2" {
		t.Fatalf("probe after a plain reload = %+v, want the replacement inputs", got)
	}
	if err := c.ReloadAll(ctx, ReasonConfig); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	if got := probe(); got.Value != "two" || got.Name != "gen2" {
		t.Fatalf("probe after ReloadAll = %+v, want the replacement inputs", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestReloadRuntimeWithRollbackRestoresInputs covers the rollback of a
// reload that carried replacement inputs: the runtime swaps the new
// generation in before the bind, so the core side has already retained
// the replacement externals and resolver when the bind fails. The
// restored generation therefore has to be rebuilt with the inputs the
// previous one was serving with — not with the failed call's, which is
// what a rollback that reloads the document alone would leave serving.
func TestReloadRuntimeWithRollbackRestoresInputs(t *testing.T) {
	dir := t.TempDir()
	binder := &binderCapability{failAt: 2, failErr: errors.New("registry is locked")}
	c, ctx := reloadTestCraft(t, dir, testCapability{}, reloadProbeCapability{}, binder)
	overlay := "resources:\n  probe:\n    kind: test.ReloadProbe\n" +
		"    settings:\n      name: \"${craft:name}\"\n" +
		"    deps:\n      db: db\n"
	if err := os.WriteFile(filepath.Join(dir, "overlay.yaml"),
		[]byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	layers := []deploy.Layer{{
		Name:     "overlay",
		Priority: 30,
		Source:   resource.Source{File: "overlay.yaml"},
		BaseDir:  dir,
	}}
	options := func(name, db string) RuntimeOptions {
		return RuntimeOptions{
			Layers:    layers,
			Values:    map[string]string{"name": name},
			Externals: []deploy.ExternalResource{deploy.NewExternal("db", "db.Pool", db)},
		}
	}
	rt, err := c.OpenRuntime(ctx, DefaultKey, options("gen1", "one"))
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	probe := func() reloadProbe {
		t.Helper()
		value, ok := rt.Resource("probe")
		if !ok {
			t.Fatal("probe resource missing")
		}
		got, ok := value.(reloadProbe)
		if !ok {
			t.Fatalf("probe resource is %T", value)
		}
		return got
	}
	if got := probe(); got.Value != "one" || got.Name != "gen1" {
		t.Fatalf("generation 1 probe = %+v, want value one, name gen1", got)
	}

	// The reload installs the replacement inputs and then fails to bind
	// the generation it built with them. A failed reload reports no
	// generation: the error, not a result, says what serves.
	result, err := c.ReloadRuntimeWith(ctx, DefaultKey, options("gen2", "two"),
		ReasonManual)
	if err == nil {
		t.Fatal("ReloadRuntimeWith bound a generation its binder refused")
	}
	if result != nil {
		t.Fatalf("ReloadRuntimeWith result = %+v, want none for a failed reload",
			result)
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("ReloadRuntimeWith = %v, want it to report the rollback", err)
	}
	if got := probe(); got.Value != "one" || got.Name != "gen1" {
		t.Fatalf("probe after the rolled-back reload = %+v, want the inputs "+
			"the previous generation was serving with", got)
	}

	// The rollback re-retained them too: the next reload rebuilds with
	// the previous generation's inputs, so the failed call cannot leak
	// into any later generation either.
	if err := c.ReloadRuntime(ctx, DefaultKey, ReasonManual); err != nil {
		t.Fatalf("ReloadRuntime: %v", err)
	}
	if got := probe(); got.Value != "one" || got.Name != "gen1" {
		t.Fatalf("probe after a plain reload = %+v, want the previous "+
			"generation's inputs", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestReloadRuntimeWithReplacesHostDecorators covers the decorator
// channels of a reload: the options' decorators are the ones the new
// generation is decorated with, a plain reload re-applies them, and
// dropping them from the options removes the decoration.
func TestReloadRuntimeWithReplacesHostDecorators(t *testing.T) {
	dir := t.TempDir()
	c, ctx := reloadTestCraft(t, dir, testCapability{})
	var first, second int
	decorator := func(calls *int) runtime.HostFactoryDecorator {
		return func(base session.HostFactory) (session.HostFactory, error) {
			*calls++
			return base, nil
		}
	}
	if _, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{
		HostDecorators: []runtime.HostFactoryDecorator{decorator(&first)},
	}); err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	if first != 1 || second != 0 {
		t.Fatalf("decorator calls after open = %d/%d, want 1/0", first, second)
	}

	if _, err := c.ReloadRuntimeWith(ctx, DefaultKey, RuntimeOptions{
		HostDecorators: []runtime.HostFactoryDecorator{decorator(&second)},
	}, ReasonManual); err != nil {
		t.Fatalf("ReloadRuntimeWith: %v", err)
	}
	if first != 1 || second != 1 {
		t.Fatalf("decorator calls after the replacement = %d/%d, want 1/1",
			first, second)
	}
	if err := c.ReloadRuntime(ctx, DefaultKey, ReasonManual); err != nil {
		t.Fatalf("ReloadRuntime: %v", err)
	}
	if first != 1 || second != 2 {
		t.Fatalf("decorator calls after a plain reload = %d/%d, want 1/2",
			first, second)
	}

	// Options without decorators remove the retained one instead of
	// leaving it on the next generation.
	if _, err := c.ReloadRuntimeWith(ctx, DefaultKey, RuntimeOptions{},
		ReasonManual); err != nil {
		t.Fatalf("ReloadRuntimeWith without decorators: %v", err)
	}
	if err := c.ReloadRuntime(ctx, DefaultKey, ReasonManual); err != nil {
		t.Fatalf("ReloadRuntime: %v", err)
	}
	if first != 1 || second != 2 {
		t.Fatalf("decorator calls after dropping them = %d/%d, want 1/2",
			first, second)
	}

	// A runtime holds one decorator: two are rejected on both entry
	// points, without decorating anything.
	two := []runtime.HostFactoryDecorator{decorator(&first), decorator(&second)}
	if _, err := c.OpenRuntime(ctx, "second", RuntimeOptions{
		HostDecorators: two,
	}); err == nil || !strings.Contains(err.Error(), "a runtime holds one") {
		t.Fatalf("OpenRuntime with two decorators = %v, want the one-decorator error",
			err)
	}
	if _, err := c.ReloadRuntimeWith(ctx, DefaultKey, RuntimeOptions{
		HostDecorators: two,
	}, ReasonManual); err == nil || !strings.Contains(err.Error(), "a runtime holds one") {
		t.Fatalf("ReloadRuntimeWith with two decorators = %v, want the one-decorator error",
			err)
	}
	if first != 1 || second != 2 {
		t.Fatalf("decorator calls after the rejected sets = %d/%d, want 1/2",
			first, second)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestReloadFailedWithKeepsPreviousOptions covers the other half of the
// replacement contract: a ReloadRuntimeWith that fails while composing
// leaves the key on the options it was serving with, so the next plain
// reload recomposes from them.
func TestReloadFailedWithKeepsPreviousOptions(t *testing.T) {
	dir := t.TempDir()
	c, ctx := reloadTestCraft(t, dir, testCapability{}, reloadProbeCapability{})
	overlay := "resources:\n  probe:\n    kind: test.ReloadProbe\n" +
		"    settings:\n      name: \"${craft:name}\"\n" +
		"    deps:\n      db: db\n"
	if err := os.WriteFile(filepath.Join(dir, "probe.yaml"),
		[]byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	layers := []deploy.Layer{{
		Name:     "probe",
		Priority: 30,
		Source:   resource.Source{File: "probe.yaml"},
		BaseDir:  dir,
	}}
	external := []deploy.ExternalResource{deploy.NewExternal("db", "db.Pool", "one")}
	rt, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{
		Layers:    layers,
		Values:    map[string]string{"name": "gen1"},
		Externals: external,
	})
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	missing := []deploy.Layer{{
		Name:     "absent",
		Priority: 30,
		Source:   resource.Source{File: "absent.yaml"},
		BaseDir:  dir,
	}}
	result, err := c.ReloadRuntimeWith(ctx, DefaultKey, RuntimeOptions{
		Layers:    missing,
		Values:    map[string]string{"name": "gen2"},
		Externals: []deploy.ExternalResource{deploy.NewExternal("db", "db.Pool", "two")},
	}, ReasonManual)
	if err == nil {
		t.Fatal("ReloadRuntimeWith with a missing layer file succeeded")
	}
	if result != nil {
		t.Fatalf("ReloadRuntimeWith result = %+v, want none for a failed reload",
			result)
	}
	if err := c.ReloadRuntime(ctx, DefaultKey, ReasonManual); err != nil {
		t.Fatalf("ReloadRuntime after the failure: %v", err)
	}
	value, ok := rt.Resource("probe")
	if !ok {
		t.Fatal("the generation composed from the previous options is not serving")
	}
	probe, ok := value.(reloadProbe)
	if !ok {
		t.Fatalf("probe resource is %T", value)
	}
	if probe.Value != "one" || probe.Name != "gen1" {
		t.Fatalf("probe after the failed reload = %+v, want value one, name gen1",
			probe)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestReloadErrorsAfterClose pins the two reload entry points to the same
// sentinels: a closed Craft is ErrCraftClosed for both, an unknown key on
// a live Craft is ErrRuntimeNotFound for both.
func TestReloadErrorsAfterClose(t *testing.T) {
	dir := t.TempDir()
	c, ctx := reloadTestCraft(t, dir, testCapability{})
	if _, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{}); err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	if err := c.ReloadRuntime(ctx, "missing", ReasonManual); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("ReloadRuntime(missing) = %v, want ErrRuntimeNotFound", err)
	}
	if _, err := c.ReloadRuntimeWith(ctx, "missing", RuntimeOptions{},
		ReasonManual); !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("ReloadRuntimeWith(missing) = %v, want ErrRuntimeNotFound", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.ReloadRuntime(ctx, DefaultKey, ReasonManual); !errors.Is(err, ErrCraftClosed) {
		t.Fatalf("ReloadRuntime after Close = %v, want ErrCraftClosed", err)
	}
	if _, err := c.ReloadRuntimeWith(ctx, DefaultKey, RuntimeOptions{},
		ReasonManual); !errors.Is(err, ErrCraftClosed) {
		t.Fatalf("ReloadRuntimeWith after Close = %v, want ErrCraftClosed", err)
	}
}

// TestReloadRejectsNilContext pins the nil-context guard of both reload
// entry points: neither a panic nor a reload, and no result.
func TestReloadRejectsNilContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, ctx := reloadTestCraft(t, dir, testCapability{})
	if _, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{}); err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	// Through a variable: passing a literal nil context is what
	// staticcheck's SA1012 forbids, and this is the one place the guard
	// is the thing under test.
	var nilCtx context.Context
	if err := c.ReloadRuntime(nilCtx, DefaultKey, ReasonManual); err == nil ||
		!strings.Contains(err.Error(), "context is required") {
		t.Fatalf("ReloadRuntime(nil) = %v, want the context error", err)
	}
	result, err := c.ReloadRuntimeWith(nilCtx, DefaultKey, RuntimeOptions{},
		ReasonManual)
	if err == nil || !strings.Contains(err.Error(), "context is required") {
		t.Fatalf("ReloadRuntimeWith(nil) = %v, want the context error", err)
	}
	if result != nil {
		t.Fatalf("ReloadRuntimeWith(nil) result = %+v, want none", result)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestReloadFailsWhenItsRuntimeIsClosedUnderIt covers the interleaving
// the write-back is skipped for: CloseRuntime and Close take the table
// lock alone, so either can take a runtime out from under a reload parked
// in its bind, after the swap committed. The reload must not report that
// generation as installed — the runtime it describes is gone — so it
// fails with the sentinel a caller would meet on the next call, and no
// craft.reload.completed reaches the plane.
func TestReloadFailsWhenItsRuntimeIsClosedUnderIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		close func(context.Context, *Craft) error
		want  error
	}{{
		name: "CloseRuntime",
		close: func(ctx context.Context, c *Craft) error {
			return c.CloseRuntime(ctx, DefaultKey)
		},
		want: ErrRuntimeNotFound,
	}, {
		name:  "Close",
		close: func(_ context.Context, c *Craft) error { return c.Close() },
		want:  ErrCraftClosed,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			entered := make(chan struct{})
			release := make(chan struct{})
			binder := &parkingBinder{
				parkAt: 2, entered: entered, release: release,
			}
			c, ctx := reloadTestCraft(t, dir, testCapability{}, binder)
			if _, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{}); err != nil {
				t.Fatalf("OpenRuntime: %v", err)
			}
			var mu sync.Mutex
			completed := false
			detach, err := c.Attach(ctx, PatternCraft(), event.SinkFunc(
				func(_ context.Context, envelope event.Envelope) error {
					mu.Lock()
					defer mu.Unlock()
					if envelope.Subject == SubjectReloadCompleted {
						completed = true
					}
					return nil
				}))
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			defer detach()

			var (
				result *runtime.ReloadResult
				reload error
			)
			done := make(chan struct{})
			go func() {
				defer close(done)
				result, reload = c.ReloadRuntimeWith(ctx, DefaultKey,
					RuntimeOptions{}, ReasonManual)
			}()
			<-entered
			// The reload has committed its swap and is inside the bind;
			// the close wins the table lock before the write-back.
			if cerr := tc.close(ctx, c); cerr != nil {
				t.Fatalf("%s: %v", tc.name, cerr)
			}
			close(release)
			<-done

			if !errors.Is(reload, tc.want) {
				t.Fatalf("ReloadRuntimeWith = %v, want %v", reload, tc.want)
			}
			if result != nil {
				t.Fatalf("ReloadRuntimeWith result = %+v, want none for a "+
					"runtime that was closed under it", result)
			}
			time.Sleep(50 * time.Millisecond) // let any published event be delivered
			mu.Lock()
			reported := completed
			mu.Unlock()
			if reported {
				t.Fatal("the reload published craft.reload.completed for a " +
					"generation nothing is bound to")
			}
		})
	}
}

// TestOpenRuntimeBindFailureLeavesTheReopenedKeyAlone covers who owns the
// entry a failed open is about to remove. OpenRuntime publishes the
// runtime before its bind runs, so the key is reachable while the bind is
// in flight: a CloseRuntime and a second open can take it and replace it.
// The cleanup of the failed open has to leave that second runtime alone —
// carrying the entry away would leave a runtime serving with nothing
// pointing at it, and a key that answers NotFound about it and refuses
// the close that would have released it.
func TestOpenRuntimeBindFailureLeavesTheReopenedKeyAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binder := &parkingBinder{
		parkAt: 1, entered: make(chan struct{}), release: make(chan struct{}),
		failErr: errors.New("bind refused"),
	}
	c, ctx := reloadTestCraft(t, dir, testCapability{}, binder)

	firstErr := make(chan error, 1)
	go func() {
		_, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{})
		firstErr <- err
	}()
	select {
	case <-binder.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the open never reached its bind")
	}
	// The runtime is published before the bind, so the key is the
	// caller's to close while the bind runs.
	parked, ok := c.Runtime(DefaultKey)
	if !ok {
		t.Fatal("the open did not publish its runtime before the bind")
	}
	if err := c.CloseRuntime(ctx, DefaultKey); err != nil {
		t.Fatalf("CloseRuntime under the parked bind: %v", err)
	}
	// The key opens again, and this open binds without being parked.
	if _, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{}); err != nil {
		t.Fatalf("OpenRuntime after the close: %v", err)
	}
	reopened, ok := c.Runtime(DefaultKey)
	if !ok {
		t.Fatal("the reopened key is not reachable")
	}
	if reopened == parked {
		t.Fatal("the reopened key still points at the parked runtime")
	}

	close(binder.release)
	if err := <-firstErr; err == nil {
		t.Fatal("the open whose bind failed reported success")
	}
	current, ok := c.Runtime(DefaultKey)
	if !ok || current != reopened {
		t.Fatalf("after the failed open the key = %v, %v, want the reopened runtime",
			current, ok)
	}
	if err := c.CloseRuntime(ctx, DefaultKey); err != nil {
		t.Fatalf("CloseRuntime after the failed open: %v", err)
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
