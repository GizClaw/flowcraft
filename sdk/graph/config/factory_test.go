package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	coregraph "github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/inference"
	inferenceconfig "github.com/GizClaw/flowcraft/sdk/inference/config"
	"github.com/GizClaw/flowcraft/sdk/tool"
	toolconfig "github.com/GizClaw/flowcraft/sdk/tool/config"
)

type recordingRuntime struct {
	calls atomic.Int32
	mu    sync.Mutex
	src   string
}

func (r *recordingRuntime) Exec(_ context.Context, _ string, source string, _ *agent.ScriptEnv) (*agent.ScriptSignal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.Add(1)
	r.src = source
	return &agent.ScriptSignal{Type: "done"}, nil
}

func loaderFor(dir string) *sdkconfig.Loader {
	return sdkconfig.NewLoader(sdkconfig.WithBaseDir(dir))
}

func TestFactorySpec(t *testing.T) {
	spec := NewFactory().Spec()
	if spec.Kind != Kind {
		t.Fatalf("kind = %q, want %q", spec.Kind, Kind)
	}
	if !spec.Capabilities.SupportsResume ||
		!spec.Capabilities.EmitsCheckpoint ||
		!spec.Capabilities.EmitsUserPrompt {
		t.Fatalf("capabilities = %+v", spec.Capabilities)
	}
	want := []agent.DepSpec{
		{Name: DepInference, Type: "inference.Assembly"},
		{Name: DepTools, Type: toolconfig.ResourceKind},
		{Name: DepWorkspace, Type: "workspace.Workspace"},
		{Name: DepSandbox, Type: "sandbox.Runner"},
		{Name: DepScriptRuntime, Type: "agent.ScriptRuntime"},
	}
	if !reflect.DeepEqual(spec.Deps, want) {
		t.Fatalf("deps = %#v, want %#v", spec.Deps, want)
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip agent.EngineSpec
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip, spec) {
		t.Fatalf("wire round trip = %#v, want %#v", roundTrip, spec)
	}
}

func TestFactoryInlineBuildAndExecute(t *testing.T) {
	rt := &recordingRuntime{}
	engine, err := NewFactory().New(context.Background(), agent.Config{
		Deps: map[string]any{DepScriptRuntime: agent.ScriptRuntime(rt)},
		Settings: inlineSettings(map[string]any{
			"name":  "simple",
			"entry": "run",
			"nodes": []any{
				map[string]any{
					"id": "run", "type": "script",
					"config": map[string]any{
						"runtime": "js",
						"source":  "signal.done()",
					},
				},
			},
			"edges": []any{},
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g, ok := engine.(*coregraph.Graph)
	if !ok {
		t.Fatalf("engine = %T, want *graph.Graph", engine)
	}
	board := agent.NewBoard()
	if _, err := g.Execute(context.Background(), agent.Run{
		Identity: agent.Identity{RunID: "run-1"},
	}, agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rt.calls.Load() != 1 {
		t.Fatalf("runtime calls = %d, want 1", rt.calls.Load())
	}
}

func TestFactoryFileBuild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "graphs", "simple.json")
	if err := os.Mkdir(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, definitionJSON("script", `{"runtime":"js","source":"ok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := &recordingRuntime{}
	engine, err := NewFactory(WithLoader(loaderFor(dir))).New(context.Background(), agent.Config{
		Deps:     map[string]any{DepScriptRuntime: agent.ScriptRuntime(rt)},
		Settings: map[string]any{"graph": map[string]any{"file": "graphs/simple.json"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := engine.(*coregraph.Graph); !ok {
		t.Fatalf("engine = %T", engine)
	}

	if _, err := NewFactory(WithLoader(loaderFor(dir))).New(context.Background(), agent.Config{
		Deps:     map[string]any{DepScriptRuntime: agent.ScriptRuntime(rt)},
		Settings: map[string]any{"graph": string(definitionJSON("script", `{"runtime":"js","source":"ok"}`))},
	}); err != nil {
		t.Fatalf("New with literal graph content: %v", err)
	}
}

func TestFactoryFileBuildYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "graphs", "simple.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`name: simple
entry: node
nodes:
  - id: node
    type: script
    config:
      runtime: js
      source: ok
edges: []
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	rt := &recordingRuntime{}
	engine, err := NewFactory(WithLoader(loaderFor(dir))).New(context.Background(), agent.Config{
		Deps:     map[string]any{DepScriptRuntime: agent.ScriptRuntime(rt)},
		Settings: map[string]any{"graph": map[string]any{"file": "graphs/simple.yaml"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := engine.(*coregraph.Graph); !ok {
		t.Fatalf("engine = %T, want *graph.Graph", engine)
	}
}

func TestFactoryFileBuildFromEmbed(t *testing.T) {
	fsys := fstest.MapFS{
		"simple.json": {Data: definitionJSON("script", `{"runtime":"js","source":"ok"}`)},
		"large.json":  {Data: make([]byte, 1<<20+1)},
	}
	factory := NewFactory(WithLoader(sdkconfig.NewLoader(sdkconfig.WithEmbed(fsys))))
	_, err := factory.New(context.Background(), agent.Config{
		Deps:     map[string]any{DepScriptRuntime: agent.ScriptRuntime(&recordingRuntime{})},
		Settings: map[string]any{"graph": map[string]any{"embed": "simple.json"}},
	})
	if err != nil {
		t.Fatalf("New with embed registry: %v", err)
	}

	_, err = factory.New(context.Background(), agent.Config{
		Settings: map[string]any{"graph": map[string]any{"embed": "large.json"}},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("oversized embed error = %v, want Validation", err)
	}
}

func TestFactoryMaterializesConfigFileRefs(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "scripts", "run.js")
	promptPath := filepath.Join(dir, "prompts", "formatter.txt")
	for _, path := range []string{scriptPath, promptPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(scriptPath, []byte("signal.done()\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptPath, []byte("You are a strict formatter.\nOutput patterns only.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	graphPath := filepath.Join(dir, "graphs", "mixed.json")
	if err := os.MkdirAll(filepath.Dir(graphPath), 0o755); err != nil {
		t.Fatal(err)
	}
	definition := map[string]any{
		"name": "mixed", "entry": "script",
		"nodes": []any{
			map[string]any{
				"id": "script", "type": "script",
				"config": map[string]any{
					"runtime": "js",
					"source":  map[string]any{"file": "scripts/run.js"},
				},
			},
			map[string]any{
				"id": "infer", "type": "inference",
				"config": map[string]any{
					"model": map[string]any{
						"id": map[string]any{"provider": "fake", "name": "model"},
					},
					"system_prompt": map[string]any{"file": "prompts/formatter.txt"},
				},
			},
		},
		"edges": []any{},
	}
	raw, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graphPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	rt := &recordingRuntime{}
	engine, err := NewFactory(WithLoader(loaderFor(dir))).New(context.Background(), agent.Config{
		Deps: map[string]any{
			DepScriptRuntime: agent.ScriptRuntime(rt),
			DepInference:     &inferenceconfig.Assembly{Runtime: &inference.Runtime{}},
		},
		Settings: map[string]any{"graph": map[string]any{"file": "graphs/mixed.json"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g, ok := engine.(*coregraph.Graph)
	if !ok {
		t.Fatalf("engine = %T, want *graph.Graph", engine)
	}
	board := agent.NewBoard()
	if _, err := g.Execute(context.Background(), agent.Run{
		Identity: agent.Identity{RunID: "run-1"},
	}, agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rt.mu.Lock()
	source := rt.src
	rt.mu.Unlock()
	if source != "signal.done()\n" {
		t.Fatalf("script source = %q, want materialized file content", source)
	}
}

func TestFactoryConfigFileRefsRejectMalformed(t *testing.T) {
	tests := map[string]map[string]any{
		"extra key": {
			"source": map[string]any{"file": "scripts/run.js", "extra": true},
		},
		"empty file": {
			"source": map[string]any{"file": "  "},
		},
		"wrong object": {
			// a "file" key turns this into a reference, so extra keys are
			// a decode error rather than content
			"source": map[string]any{"file": "scripts/run.js", "bogus": "x"},
		},
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			settings := map[string]any{
				"name": "simple", "entry": "node",
				"nodes": []any{map[string]any{
					"id": "node", "type": "script",
					"config": map[string]any{"runtime": "js", "source": config["source"]},
				}},
				"edges": []any{},
			}
			_, err := NewFactory().New(context.Background(), agent.Config{
				Deps:     map[string]any{DepScriptRuntime: agent.ScriptRuntime(&recordingRuntime{})},
				Settings: inlineSettings(settings),
			})
			if !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want Validation", err)
			}
		})
	}
}

func TestFactoryConfigFileRefsMissingFile(t *testing.T) {
	_, err := NewFactory().New(context.Background(), agent.Config{
		Deps: map[string]any{DepScriptRuntime: agent.ScriptRuntime(&recordingRuntime{})},
		Settings: inlineSettings(map[string]any{
			"name": "simple", "entry": "node",
			"nodes": []any{map[string]any{
				"id": "node", "type": "script",
				"config": map[string]any{
					"runtime": "js",
					"source":  map[string]any{"file": "scripts/missing.js"},
				},
			}},
			"edges": []any{},
		}),
	})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("error = %v, want NotFound", err)
	}
}

func TestFactoryGraphSourceValidation(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]any
	}{
		{name: "missing", settings: map[string]any{}},
		{name: "both", settings: map[string]any{"graph": map[string]any{
			"file": "a.json", "inline": validDefinition("script", map[string]any{"runtime": "js", "source": "ok"}),
		}}},
		{name: "unknown settings", settings: map[string]any{
			"graph": map[string]any{"inline": validDefinition("script", map[string]any{"runtime": "js", "source": "ok"})},
			"typo":  true,
		}},
		{name: "unknown graph", settings: map[string]any{"graph": map[string]any{
			"inline": validDefinition("script", map[string]any{"runtime": "js", "source": "ok"}),
			"typo":   true,
		}}},
		{name: "unknown definition", settings: map[string]any{"graph": map[string]any{"inline": map[string]any{
			"name": "g", "entry": "n", "nodes": []any{map[string]any{
				"id": "n", "type": "script", "config": map[string]any{"runtime": "js", "source": "ok"},
			}}, "edges": []any{}, "typo": true,
		}}}},
		{name: "unknown build", settings: mergeSettings(inlineSettings(validDefinition("script", map[string]any{"runtime": "js", "source": "ok"})), map[string]any{
			"build": map[string]any{"typo": true},
		})},
		{name: "unknown parallel", settings: mergeSettings(inlineSettings(validDefinition("script", map[string]any{"runtime": "js", "source": "ok"})), map[string]any{
			"build": map[string]any{"parallel": map[string]any{"typo": true}},
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewFactory().New(context.Background(), agent.Config{
				Deps:     map[string]any{DepScriptRuntime: agent.ScriptRuntime(&recordingRuntime{})},
				Settings: tt.settings,
			})
			if !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want Validation", err)
			}
		})
	}
}

func TestFactoryFileErrors(t *testing.T) {
	dir := t.TempDir()
	rt := agent.ScriptRuntime(&recordingRuntime{})
	newWithFile := func(file string) error {
		_, err := NewFactory(WithLoader(loaderFor(dir))).New(context.Background(), agent.Config{
			Deps:     map[string]any{DepScriptRuntime: rt},
			Settings: map[string]any{"graph": map[string]any{"file": file}},
		})
		return err
	}

	if err := newWithFile("missing.json"); !errdefs.IsNotFound(err) {
		t.Fatalf("missing error = %v, want NotFound", err)
	}
	if err := newWithFile("../escape.json"); !errdefs.IsForbidden(err) {
		t.Fatalf("escape error = %v, want Forbidden", err)
	}
	if err := newWithFile("."); !errdefs.IsValidation(err) {
		t.Fatalf("directory error = %v, want Validation", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trailing.json"),
		append(definitionJSON("script", `{"runtime":"js","source":"ok"}`), []byte(` {}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newWithFile("trailing.json"); !errdefs.IsValidation(err) {
		t.Fatalf("trailing error = %v, want Validation", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unknown.yaml"),
		[]byte("name: simple\nentry: node\nnodes: []\nedges: []\nsurprise: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newWithFile("unknown.yaml"); !errdefs.IsValidation(err) {
		t.Fatalf("unknown yaml field error = %v, want Validation", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "multi.yaml"),
		[]byte("name: simple\nentry: node\nnodes: []\nedges: []\n---\nname: second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newWithFile("multi.yaml"); !errdefs.IsValidation(err) {
		t.Fatalf("multi yaml document error = %v, want Validation", err)
	}
	large := make([]byte, 1<<20+1)
	if err := os.WriteFile(filepath.Join(dir, "large.json"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newWithFile("large.json"); !errdefs.IsValidation(err) {
		t.Fatalf("large error = %v, want Validation", err)
	}
}

func TestFactoryBuildSettingsValidation(t *testing.T) {
	base := inlineSettings(validDefinition("script", map[string]any{"runtime": "js", "source": "ok"}))
	tests := []map[string]any{
		{"timeout": "-1s"},
		{"timeout": "not-duration"},
		{"run_end_publish_timeout": "0s"},
		{"run_end_publish_timeout": "-1s"},
		{"run_end_publish_timeout": "not-duration"},
		{"max_node_retries": -1},
		{"parallel": map[string]any{"enabled": true, "branch_timeout": "-1s"}},
		{"parallel": map[string]any{"enabled": true, "max_concurrency": -1}},
		{"parallel": map[string]any{"enabled": true, "max_branches": -1}},
		{"parallel": map[string]any{"enabled": true, "merge_strategy": "random"}},
	}
	for _, build := range tests {
		_, err := NewFactory().New(context.Background(), agent.Config{
			Deps:     map[string]any{DepScriptRuntime: agent.ScriptRuntime(&recordingRuntime{})},
			Settings: mergeSettings(base, map[string]any{"build": build}),
		})
		if !errdefs.IsValidation(err) {
			t.Fatalf("build %#v: error = %v, want Validation", build, err)
		}
	}

	engine, err := NewFactory().New(context.Background(), agent.Config{
		Deps: map[string]any{DepScriptRuntime: agent.ScriptRuntime(&recordingRuntime{})},
		Settings: mergeSettings(base, map[string]any{"build": map[string]any{
			"max_iterations":          7,
			"timeout":                 "1s",
			"run_end_publish_timeout": "2s",
			"parallel": map[string]any{
				"enabled":         true,
				"branch_timeout":  "250ms",
				"max_concurrency": 2,
				"max_branches":    3,
				"merge_strategy":  "last_write_wins",
			},
		}}),
	})
	if err != nil {
		t.Fatalf("valid build: %v", err)
	}
	stats := engine.(*coregraph.Graph).Stats()
	if stats.MaxIterations != 7 || !stats.ParallelEnabled {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestFactoryConditionalDependencies(t *testing.T) {
	tests := []struct {
		nodeType string
		config   map[string]any
		depName  string
	}{
		{nodeType: "inference", config: map[string]any{}, depName: DepInference},
		{nodeType: "tool", config: map[string]any{}, depName: DepTools},
		{nodeType: "script", config: map[string]any{"runtime": "js", "source": "ok"}, depName: DepScriptRuntime},
	}
	for _, tt := range tests {
		t.Run(tt.nodeType, func(t *testing.T) {
			_, err := NewFactory().New(context.Background(), agent.Config{
				Settings: inlineSettings(validDefinition(tt.nodeType, tt.config)),
			})
			if !errdefs.IsNotFound(err) {
				t.Fatalf("error = %v, want NotFound", err)
			}
		})
	}

	// Optional means conditional: a graph without a script node does not
	// require a script runtime.
	assembly := &inferenceconfig.Assembly{Runtime: &inference.Runtime{}}
	if _, err := NewFactory().New(context.Background(), agent.Config{
		Deps: map[string]any{DepInference: assembly},
		Settings: inlineSettings(validDefinition("inference", map[string]any{
			"model": map[string]any{
				"id": map[string]any{"provider": "fake", "name": "model"},
			},
		})),
	}); err != nil {
		t.Fatalf("inference-only graph unexpectedly requires other deps: %v", err)
	}
}

func TestFactoryInferenceConditionalDependencies(t *testing.T) {
	inferenceAssembly := &inferenceconfig.Assembly{Runtime: &inference.Runtime{}}

	_, err := NewFactory().New(context.Background(), agent.Config{
		Deps:     map[string]any{DepInference: inferenceAssembly},
		Settings: inlineSettings(validDefinition("inference", map[string]any{})),
	})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("router error = %v, want NotFound", err)
	}

	withTools := map[string]any{
		"model": map[string]any{
			"id": map[string]any{"provider": "fake", "name": "model"},
		},
		"tools": []any{"missing"},
	}
	_, err = NewFactory().New(context.Background(), agent.Config{
		Deps:     map[string]any{DepInference: inferenceAssembly},
		Settings: inlineSettings(validDefinition("inference", withTools)),
	})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("tools dep error = %v, want NotFound", err)
	}

	registry := tool.NewRegistry()
	toolAssembly := &toolconfig.Assembly{
		Executor: tool.NewExecutor(registry),
		Catalog:  registry,
	}
	_, err = NewFactory().New(context.Background(), agent.Config{
		Deps: map[string]any{
			DepInference: inferenceAssembly,
			DepTools:     toolAssembly,
		},
		Settings: inlineSettings(validDefinition("inference", withTools)),
	})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("unknown tool error = %v, want NotFound", err)
	}
}

func TestFactoryInferenceDependencyScanPreservesBoardReferences(t *testing.T) {
	registry := tool.NewRegistry()
	_, err := NewFactory().New(context.Background(), agent.Config{
		Deps: map[string]any{
			DepInference: &inferenceconfig.Assembly{Runtime: &inference.Runtime{}},
			DepTools: &toolconfig.Assembly{
				Executor: tool.NewExecutor(registry),
				Catalog:  registry,
			},
		},
		Settings: inlineSettings(validDefinition("inference", map[string]any{
			"model":       "${board.model}",
			"temperature": "${board.temperature}",
			"tools":       []any{"${board.tool}"},
		})),
	})
	if err != nil {
		t.Fatalf("New with board references: %v", err)
	}
}

func TestFactoryRejectsIncompleteAssemblies(t *testing.T) {
	for name, deps := range map[string]map[string]any{
		"inference runtime": {
			DepInference: &inferenceconfig.Assembly{},
		},
		"tool executor": {
			DepTools: &toolconfig.Assembly{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewFactory().New(context.Background(), agent.Config{
				Deps:     deps,
				Settings: inlineSettings(validDefinition("custom", map[string]any{})),
			})
			if !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want Validation", err)
			}
		})
	}
}

func TestFactoryDependencyTypeAndTypedNil(t *testing.T) {
	var nilAssembly *inferenceconfig.Assembly
	for name, value := range map[string]any{
		"wrong type": "not-an-assembly",
		"typed nil":  nilAssembly,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewFactory().New(context.Background(), agent.Config{
				Deps:     map[string]any{DepInference: value},
				Settings: inlineSettings(validDefinition("inference", map[string]any{})),
			})
			if !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want Validation", err)
			}
		})
	}

	var nilRuntime *recordingRuntime
	_, err := NewFactory().New(context.Background(), agent.Config{
		Deps:     map[string]any{DepScriptRuntime: nilRuntime},
		Settings: inlineSettings(validDefinition("script", map[string]any{"runtime": "js", "source": "ok"})),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("typed nil runtime error = %v, want Validation", err)
	}

	_, err = NewFactory().New(context.Background(), agent.Config{
		Deps:     map[string]any{"event_bus": struct{}{}},
		Settings: inlineSettings(validDefinition("custom", map[string]any{})),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("unknown dep error = %v, want Validation", err)
	}
}

func TestFactoryScriptRuntimeName(t *testing.T) {
	def := validDefinition("script", map[string]any{"runtime": "lua", "source": "ok"})
	_, err := NewFactory().New(context.Background(), agent.Config{
		Deps:     map[string]any{DepScriptRuntime: agent.ScriptRuntime(&recordingRuntime{})},
		Settings: inlineSettings(def),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("mismatch error = %v, want Validation", err)
	}
	if _, err := NewFactory().New(context.Background(), agent.Config{
		Deps: map[string]any{DepScriptRuntime: agent.ScriptRuntime(&recordingRuntime{})},
		Settings: mergeSettings(inlineSettings(def), map[string]any{
			"script_runtime_name": "lua",
		}),
	}); err != nil {
		t.Fatalf("matching runtime name: %v", err)
	}
}

func TestFactoryUnknownNodeDelegatesToBuild(t *testing.T) {
	_, err := NewFactory().New(context.Background(), agent.Config{
		Settings: inlineSettings(validDefinition("custom", map[string]any{})),
	})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("error = %v, want graph.Build NotFound", err)
	}
}

func TestFactoryNewRegistryIsolationAndConcurrency(t *testing.T) {
	factory := NewFactory()
	const count = 24
	var wg sync.WaitGroup
	errs := make(chan error, count)
	engines := make(chan agent.Engine, count)
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			engine, err := factory.New(context.Background(), agent.Config{
				Deps:     map[string]any{DepScriptRuntime: agent.ScriptRuntime(&recordingRuntime{})},
				Settings: inlineSettings(validDefinition("script", map[string]any{"runtime": "js", "source": "ok"})),
			})
			if err != nil {
				errs <- err
				return
			}
			engines <- engine
		}()
	}
	wg.Wait()
	close(errs)
	close(engines)
	for err := range errs {
		t.Errorf("concurrent New: %v", err)
	}
	seen := map[*coregraph.Graph]bool{}
	for engine := range engines {
		g := engine.(*coregraph.Graph)
		if seen[g] {
			t.Fatal("New returned the same graph instance twice")
		}
		seen[g] = true
	}
	if len(seen) != count {
		t.Fatalf("built %d graphs, want %d", len(seen), count)
	}
}

func inlineSettings(def map[string]any) map[string]any {
	raw, err := json.Marshal(def)
	if err != nil {
		panic(err)
	}
	return map[string]any{"graph": string(raw)}
}

func validDefinition(nodeType string, config map[string]any) map[string]any {
	return map[string]any{
		"name": "simple", "entry": "node",
		"nodes": []any{map[string]any{"id": "node", "type": nodeType, "config": config}},
		"edges": []any{},
	}
}

func mergeSettings(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func definitionJSON(nodeType, config string) []byte {
	return []byte(`{"name":"simple","entry":"node","nodes":[{"id":"node","type":"` +
		nodeType + `","config":` + config + `}],"edges":[]}`)
}
