package assembly_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/engine"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/assembly"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

func TestDecodeYAMLAndJSON(t *testing.T) {
	yamlManifest := `
id: demo
workspace:
  backend: filesystem
  root: ./state
recall:
  backend: workspace
  asyncSemantic: true
  ops:
    enabled: true
knowledge:
  backend: workspace
  datasets:
    - id: docs
history:
  kind: buffer
agents:
  - name: primary
    engine: test
    tools:
      - recall.search
llm:
  default: openai-main
`
	jsonManifest := `{
  "id": "demo",
  "workspace": {"backend": "filesystem", "root": "./state"},
  "recall": {"backend": "workspace", "asyncSemantic": true, "ops": {"enabled": true}},
  "knowledge": {"backend": "workspace", "datasets": [{"id": "docs"}]},
  "history": {"kind": "buffer"},
  "agents": [{"name": "primary", "engine": "test", "tools": ["recall.search"]}],
  "llm": {"default": "openai-main"}
}`

	gotYAML, err := assembly.Decode(strings.NewReader(yamlManifest))
	if err != nil {
		t.Fatalf("Decode yaml: %v", err)
	}
	gotJSON, err := assembly.Decode(strings.NewReader(jsonManifest))
	if err != nil {
		t.Fatalf("Decode json: %v", err)
	}
	if gotYAML.ID != gotJSON.ID ||
		gotYAML.Recall == nil || gotJSON.Recall == nil ||
		gotYAML.Recall.Backend != gotJSON.Recall.Backend ||
		gotYAML.Recall.AsyncSemantic != gotJSON.Recall.AsyncSemantic ||
		gotYAML.Agents[0].Engine != gotJSON.Agents[0].Engine {
		t.Fatalf("yaml/json decode mismatch:\nyaml=%+v\njson=%+v", gotYAML, gotJSON)
	}
}

func TestBuildRequiresEngineFactory(t *testing.T) {
	_, err := assembly.Build(context.Background(), assembly.Manifest{
		ID:     "demo",
		Agents: []assembly.AgentSpec{{Name: "primary", Engine: "test"}},
	})
	if err == nil {
		t.Fatal("Build without engine factory should fail")
	}
	if !strings.Contains(err.Error(), "engine factory") {
		t.Fatalf("err = %v, want engine factory validation", err)
	}
}

func TestDecodeAndBuildWithRuntimeDefaults(t *testing.T) {
	defaults := assembly.Defaults{
		Workspace: assembly.FilesystemWorkspaceBackend(),
		Recall:    assembly.WorkspaceRecallBackend(),
		Knowledge: assembly.WorkspaceKnowledgeBackend(),
	}
	manifest := `
id: demo
workspace:
  root: ` + t.TempDir() + `
recall: {}
knowledge: {}
agents:
  - name: primary
    engine: test
    tools:
      - recall.search
      - knowledge.search
`
	m, err := assembly.DecodeWithDefaults(strings.NewReader(manifest), defaults)
	if err != nil {
		t.Fatalf("DecodeWithDefaults: %v", err)
	}
	if _, err := assembly.Decode(strings.NewReader(manifest)); err == nil {
		t.Fatal("Decode without filesystem default should reject workspace.root without backend")
	}

	a, err := assembly.Build(context.Background(), m, assembly.WithCatalog(newTestCatalog(t)), assembly.WithDefaults(defaults))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.Recall == nil || a.Knowledge == nil {
		t.Fatalf("runtime defaults did not assemble recall/knowledge: %+v", a)
	}
	for _, name := range []string{assembly.ToolRecallSearch, assembly.ToolKnowledgeSearch} {
		if _, ok := a.Tools.Get(name); !ok {
			t.Fatalf("tool %q not registered", name)
		}
	}
}

func TestWorkspaceRecallBackendUsesRecallSubWorkspace(t *testing.T) {
	root := t.TempDir()
	defaults := assembly.Defaults{
		Workspace: assembly.FilesystemWorkspaceBackend(),
		Recall:    assembly.WorkspaceRecallBackend(),
	}
	manifest := `
id: demo
workspace:
  root: ` + root + `
recall: {}
agents:
  - name: primary
    engine: test
`
	m, err := assembly.DecodeWithDefaults(strings.NewReader(manifest), defaults)
	if err != nil {
		t.Fatalf("DecodeWithDefaults: %v", err)
	}
	a, err := assembly.Build(context.Background(), m, assembly.WithCatalog(newTestCatalog(t)), assembly.WithDefaults(defaults))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if _, err := a.Recall.Save(context.Background(), recall.Scope{RuntimeID: "rt", UserID: "u1"}, recall.SaveRequest{
		Facts: []recall.TemporalFact{{Kind: recall.FactNote, Content: "alpha"}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "recall", "state.json")); err != nil {
		t.Fatalf("recall state path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("root state path err = %v, want not exist", err)
	}
}

func TestCustomWorkspaceBackendCanBeDefaultOrCatalogEntry(t *testing.T) {
	cat := newTestCatalog(t)
	custom := &testWorkspaceBackend{ws: sdkworkspace.NewMemWorkspace()}
	if err := cat.RegisterWorkspaceBackend("custom", custom); err != nil {
		t.Fatalf("RegisterWorkspaceBackend: %v", err)
	}
	manifest := `
id: demo
workspace:
  backend: custom
agents:
  - name: primary
    engine: test
`
	m, err := assembly.DecodeWithCatalog(strings.NewReader(manifest), assembly.DefaultDefaults(), cat)
	if err != nil {
		t.Fatalf("DecodeWithCatalog: %v", err)
	}
	a, err := assembly.Build(context.Background(), m, assembly.WithCatalog(cat))
	if err != nil {
		t.Fatalf("Build custom catalog backend: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.Workspace != custom.ws {
		t.Fatal("catalog workspace backend was not used")
	}

	defaultBackend := &testWorkspaceBackend{ws: sdkworkspace.NewMemWorkspace()}
	a2, err := assembly.Build(context.Background(), assembly.Manifest{
		ID:     "demo-default",
		Agents: []assembly.AgentSpec{{Name: "primary", Engine: "test"}},
	}, assembly.WithCatalog(cat), assembly.WithDefaults(assembly.Defaults{Workspace: defaultBackend}))
	if err != nil {
		t.Fatalf("Build custom default backend: %v", err)
	}
	t.Cleanup(func() { _ = a2.Close() })
	if a2.Workspace != defaultBackend.ws {
		t.Fatal("default workspace backend was not used")
	}
}

func TestBuildMinimalCaptain(t *testing.T) {
	cat := newTestCatalog(t)
	a, err := assembly.Build(context.Background(), assembly.Manifest{
		ID:     "demo",
		Agents: []assembly.AgentSpec{{Name: "primary", Engine: "test"}},
	}, assembly.WithCatalog(cat))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.Captain == nil {
		t.Fatal("Captain is nil")
	}
	if a.Workspace == nil || a.Tools == nil {
		t.Fatalf("workspace/tools not wired: %+v", a)
	}
}

type testWorkspaceBackend struct {
	ws sdkworkspace.Workspace
}

func (b *testWorkspaceBackend) ValidateWorkspace(assembly.WorkspaceSpec) error { return nil }

func (b *testWorkspaceBackend) BuildWorkspace(context.Context, assembly.WorkspaceSpec) (assembly.WorkspaceResource, error) {
	return assembly.WorkspaceResource{
		Workspace:    b.ws,
		SessionStore: vessel.NewMemorySessionStore(),
	}, nil
}

func TestBuildWorkspaceRecallKnowledgeToolsAndOps(t *testing.T) {
	cat := newTestCatalog(t)
	a, err := assembly.Build(context.Background(), assembly.Manifest{
		ID:        "demo",
		Workspace: assembly.WorkspaceSpec{Backend: assembly.WorkspaceBackendFilesystem, Root: t.TempDir()},
		Recall: &assembly.RecallSpec{
			Backend:       assembly.RecallBackendWorkspace,
			AsyncSemantic: true,
			Ops:           assembly.OpsSpec{Enabled: true},
		},
		Knowledge: &assembly.KnowledgeSpec{
			Backend: assembly.KnowledgeBackendWorkspace,
			Datasets: []assembly.DatasetSpec{
				{ID: "docs"},
			},
		},
		History: &assembly.HistorySpec{Kind: assembly.HistoryKindBuffer},
		Agents: []assembly.AgentSpec{{
			Name:   "primary",
			Engine: "test",
			Tools:  []string{assembly.ToolRecallSave, assembly.ToolRecallSearch, assembly.ToolKnowledgePut, assembly.ToolKnowledgeSearch},
		}},
	}, assembly.WithCatalog(cat))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if err := a.StartOps(context.Background()); err != nil {
		t.Fatalf("StartOps: %v", err)
	}
	if a.OpsSupervisor == nil {
		t.Fatal("OpsSupervisor is nil after StartOps")
	}

	execTool(t, a, assembly.ToolRecallSave, map[string]any{
		"content": "Alice prefers green tea.",
		"user_id": "alice",
	})
	if _, err := a.OpsRunner.DrainScopes(context.Background(), []recall.Scope{{RuntimeID: "demo", UserID: "alice"}}); err != nil {
		t.Fatalf("DrainScopes: %v", err)
	}
	recallOut := execTool(t, a, assembly.ToolRecallSearch, map[string]any{
		"query":   "green tea",
		"user_id": "alice",
	})
	if !strings.Contains(recallOut, "green tea") {
		t.Fatalf("recall search output missing saved fact: %s", recallOut)
	}

	execTool(t, a, assembly.ToolKnowledgePut, map[string]any{
		"dataset_id": "docs",
		"name":       "guide.md",
		"content":    "FlowCraft vessels can assemble recall and knowledge from manifests.",
	})
	knowledgeOut := execTool(t, a, assembly.ToolKnowledgeSearch, map[string]any{
		"query":      "assemble recall knowledge",
		"dataset_id": "docs",
		"scope":      "single",
	})
	if !strings.Contains(knowledgeOut, "FlowCraft vessels") {
		t.Fatalf("knowledge search output missing document: %s", knowledgeOut)
	}
}

func newTestCatalog(t *testing.T) *assembly.Catalog {
	t.Helper()
	cat := assembly.NewCatalog()
	err := cat.RegisterEngine("test", func(_ spec.Agent, _ vessel.Deps) (engine.Engine, error) {
		return engine.EngineFunc(nil), nil
	})
	if err != nil {
		t.Fatalf("RegisterEngine: %v", err)
	}
	return cat
}

func execTool(t *testing.T, a *assembly.Assembly, name string, args map[string]any) string {
	t.Helper()
	tool, ok := a.Tools.Get(name)
	if !ok {
		t.Fatalf("tool %q not registered", name)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), string(raw))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}
