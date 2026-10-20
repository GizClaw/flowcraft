package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
)

// staticProvider is a SourceProvider with a fixed source list, standing
// in for a plugin host.
type staticProvider struct{ sources []ExtraSource }

func (p staticProvider) HookSources() []ExtraSource { return p.sources }

// settingsFor encodes a runner settings subtree.
func settingsFor(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	return raw
}

func TestRunnerFactoryBuildsLoadedManager(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	out := filepath.Join(t.TempDir(), "resource.out")
	path := writeHooks(t, `{
		"hooks": {"PreToolUse": [{"hooks": [{"command": "`+appendHook(out)+`"}]}]}
	}`)
	value, err := NewFactory().New(context.Background(), resource.NewInput(settingsFor(t, path), nil, nil, nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	manager := value
	if manager.Path() != path {
		t.Fatalf("manager path = %q, want %q", manager.Path(), path)
	}
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{"tool": "x"})
	waitForContent(t, out)
}

func TestRunnerFactoryRequiresPath(t *testing.T) {
	t.Parallel()
	for _, settings := range []string{`{}`, `{"path": "  "}`} {
		_, err := NewFactory().New(context.Background(), resource.NewInput([]byte(settings), nil, nil, nil))
		if err == nil {
			t.Fatalf("settings %s: a missing path must fail the build", settings)
		}
	}
}

func TestRunnerFactoryRejectsUnknownSettings(t *testing.T) {
	t.Parallel()
	_, err := NewFactory().New(context.Background(), resource.NewInput([]byte(`{"path": "/tmp/hooks.json", "matcher": "^x$"}`), nil, nil, nil))
	if err == nil {
		t.Fatal("an unknown settings field must fail the build")
	}
}

func TestRunnerFactoryAllowsMissingFile(t *testing.T) {
	t.Parallel()
	value, err := NewFactory().New(context.Background(), resource.NewInput(settingsFor(t, filepath.Join(t.TempDir(), "absent.json")), nil, nil, nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if manager := value; !manager.Empty() {
		t.Fatal("a missing hooks.json must yield an empty manager, not an error")
	}
}

func TestRunnerFactoryWiresSourceProvider(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	pluginDir := t.TempDir()
	out := filepath.Join(pluginDir, "plugin.out")
	if err := os.WriteFile(filepath.Join(pluginDir, "hooks.json"), []byte(`{
		"hooks": {"PreToolUse": [{"hooks": [{"command": "`+appendHook(out)+`"}]}]}
	}`), 0o600); err != nil {
		t.Fatalf("write plugin hooks.json: %v", err)
	}
	provider := staticProvider{sources: []ExtraSource{{
		Path: filepath.Join(pluginDir, "hooks.json"),
		Dir:  pluginDir,
	}}}
	value, err := NewFactory(WithSources(provider)).New(
		context.Background(),
		resource.NewInput(settingsFor(t, filepath.Join(t.TempDir(), "absent.json")), nil, nil, nil),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	manager := value
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{
		"tool":       "exec_command",
		"tool_input": map[string]any{"command": "secret"},
	})
	data := waitForContent(t, out)
	if strings.Contains(string(data), "tool_input") {
		t.Fatalf("provider sources are untrusted by default, payload = %s", data)
	}
}

func TestRunnerFactoryReadsSourceDep(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	pluginDir := t.TempDir()
	out := filepath.Join(pluginDir, "plugin.out")
	if err := os.WriteFile(filepath.Join(pluginDir, "hooks.json"), []byte(`{
		"hooks": {"PreToolUse": [{"hooks": [{"command": "`+appendHook(out)+`"}]}]}
	}`), 0o600); err != nil {
		t.Fatalf("write plugin hooks.json: %v", err)
	}
	factory := NewFactory(WithSourceDep[SourceProvider]("plugins"))
	deps := factory.Spec().Deps
	if len(deps) != 1 || deps[0].Name != "plugins" ||
		deps[0].Type != reflect.TypeFor[SourceProvider]() || deps[0].Required {
		t.Fatalf("Spec deps = %+v, want the optional plugins dep", deps)
	}
	value, err := factory.New(context.Background(), resource.NewInput(settingsFor(t, filepath.Join(t.TempDir(), "absent.json")), map[string]any{"plugins": staticProvider{sources: []ExtraSource{{
		Path: filepath.Join(pluginDir, "hooks.json"),
		Dir:  pluginDir,
	}}}}, nil, nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	manager := value
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{"tool": "x"})
	waitForContent(t, out)
}

func TestRunnerFactoryRejectsWrongSourceDep(t *testing.T) {
	t.Parallel()
	_, err := NewFactory(WithSourceDep[SourceProvider]("plugins")).New(
		context.Background(),
		resource.NewInput(settingsFor(t, filepath.Join(t.TempDir(), "absent.json")), map[string]any{"plugins": "not a provider"}, nil, nil),
	)
	if err == nil {
		t.Fatal("a dep that is not a SourceProvider must fail the build")
	}
	if !strings.Contains(err.Error(), "hooks.SourceProvider") {
		t.Fatalf("error = %v, want it to name the wanted interface", err)
	}
}

func TestRunnerFactorySpecWithoutSource(t *testing.T) {
	t.Parallel()
	spec := NewFactory().Spec()
	if spec.Kind != ResourceKind || spec.Impl != ImplLocal {
		t.Fatalf("Spec = %+v, want %s/%s", spec, ResourceKind, ImplLocal)
	}
	if len(spec.Deps) != 0 {
		t.Fatalf("Spec deps = %+v, want none without a source dep", spec.Deps)
	}
}

func TestObserverFactorySpec(t *testing.T) {
	t.Parallel()
	spec := observerFactory{}.Spec()
	if spec.Kind != ObserverResourceKind || spec.Impl != ImplLocal {
		t.Fatalf("Spec = %+v, want %s/%s", spec, ObserverResourceKind, ImplLocal)
	}
	want := map[string]reflect.Type{EventBusDep: reflect.TypeFor[event.Bus](), RunnerDep: reflect.TypeFor[*Manager]()}
	if len(spec.Deps) != len(want) {
		t.Fatalf("Spec deps = %+v, want %v", spec.Deps, want)
	}
	for _, dep := range spec.Deps {
		if want[dep.Name] != dep.Type || !dep.Required {
			t.Fatalf("dep %+v, want required %s", dep, want[dep.Name])
		}
	}
}

func TestRegisterRegistersBothKinds(t *testing.T) {
	t.Parallel()
	registry := resource.NewRegistry()
	if err := Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, key := range []resource.Key{
		{Kind: ResourceKind, Impl: ImplLocal},
		{Kind: ObserverResourceKind, Impl: ImplLocal},
	} {
		if _, ok := registry.Lookup(key.Kind, key.Impl); !ok {
			t.Fatalf("%s/%s is not registered", key.Kind, key.Impl)
		}
	}
}

func TestRegisterRejectsAnIncompleteSourceDep(t *testing.T) {
	t.Parallel()
	registry := resource.NewRegistry()
	// The contract comes from the type parameter now, so the only way a
	// source dep can be half-declared is an empty name — caught by spec
	// validation at registration, not silently left to build time.
	if err := Register(registry, WithSourceDep[SourceProvider]("")); err == nil {
		t.Fatal("an empty source dep name must fail registration")
	}
}

func TestObserverFactoryForwardsTransitions(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	manager, start, _ := startsAndStops(t)
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	value, err := observerFactory{}.New(context.Background(), resource.NewTestInput(map[string]any{EventBusDep: bus, RunnerDep: manager}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	observer := value
	if err := observer.Wire(context.Background()); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	t.Cleanup(func() { _ = observer.Close() })

	board := newTestBoard(t, bus)
	id := submitCard(t, board)
	if !board.ClaimCard(id, "researcher") {
		t.Fatal("ClaimCard: card was not claimable")
	}
	waitForContent(t, start)
}

func TestObserverFactoryValidatesDeps(t *testing.T) {
	t.Parallel()
	manager, _, _ := startsAndStops(t)
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	runnerOnly := map[string]any{RunnerDep: manager}
	busOnly := map[string]any{EventBusDep: bus}
	for name, deps := range map[string]map[string]any{
		"no deps":     nil,
		"events only": busOnly,
		"runner only": runnerOnly,
		"wrong events": {
			EventBusDep: "not a bus",
			RunnerDep:   manager,
		},
		"wrong runner": {
			EventBusDep: bus,
			RunnerDep:   "not a manager",
		},
	} {
		if _, err := (observerFactory{}).New(
			context.Background(), resource.NewTestInput(deps),
		); err == nil {
			t.Fatalf("%s: New must fail", name)
		}
	}
}
