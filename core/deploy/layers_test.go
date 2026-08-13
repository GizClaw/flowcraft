package deploy_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/resource"
)

func inlineSource(yaml string) resource.Source {
	return resource.Source{Inline: []byte(yaml)}
}

func TestLoadLayersMergeAndProvenance(t *testing.T) {
	layers := []deploy.Layer{
		{
			Priority: 0,
			Name:     "base",
			Source: inlineSource(`version: v1
resources:
  fs:
    kind: workspace.Workspace
    impl: local
    settings: {root: /var/flowcraft}
agents:
  researcher:
    card: {name: 研究员}
    tools: [search]
`),
		},
		{
			Priority: 1,
			Name:     "project",
			Source: inlineSource(`resources:
  fs:
    settings: {root: ./data, nested: {a: 1}}
agents:
  researcher:
    card: {name: 项目研究员}
    tools: [search, fetch]
`),
		},
	}
	doc, provenance, err := deploy.LoadLayers(context.Background(), layers)
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	fs := doc.Resources["fs"]
	if fs.Kind != "workspace.Workspace" || fs.Impl != "local" {
		t.Fatalf("fs = %+v", fs)
	}
	var settings struct {
		Root   string         `json:"root"`
		Nested map[string]int `json:"nested"`
	}
	if err := resource.DecodeSettings(&settings, fs.Settings); err != nil {
		t.Fatal(err)
	}
	if settings.Root != "./data" || settings.Nested["a"] != 1 {
		t.Fatalf("settings = %+v", settings)
	}
	agentDef := doc.Agents["researcher"]
	if agentDef.Card.Name != "项目研究员" {
		t.Fatalf("card name = %q", agentDef.Card.Name)
	}
	if len(agentDef.Tools) != 2 || agentDef.Tools[0] != "search" || agentDef.Tools[1] != "fetch" {
		t.Fatalf("tools = %v, want replaced [search fetch]", agentDef.Tools)
	}
	if ref := provenance.Resources["fs"]; ref.Priority != 1 || ref.Name != "project" {
		t.Fatalf("fs provenance = %+v", ref)
	}
	if ref := provenance.Agents["researcher"]; ref.Priority != 1 {
		t.Fatalf("agent provenance = %+v", ref)
	}
}

func TestLoadLayersMergeRuntime(t *testing.T) {
	layers := []deploy.Layer{
		{
			Priority: 0,
			Name:     "base",
			Source: inlineSource(`version: v1
resources: {}
runtime:
  event_bus: events
  sessions:
    idle_timeout: 10m
`),
		},
		{
			Priority: 1,
			Name:     "project",
			Source: inlineSource(`runtime:
  sessions:
    idle_timeout: 1m
`),
		},
	}
	doc, _, err := deploy.LoadLayers(context.Background(), layers)
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if doc.Runtime == nil {
		t.Fatal("merged document has no runtime section")
	}
	var runtime struct {
		EventBus string `json:"event_bus"`
		Sessions struct {
			IdleTimeout string `json:"idle_timeout"`
		} `json:"sessions"`
	}
	if err := resource.DecodeSettings(&runtime, json.RawMessage(*doc.Runtime)); err != nil {
		t.Fatalf("decode runtime: %v", err)
	}
	if runtime.EventBus != "events" {
		t.Fatalf("event_bus = %q, want events", runtime.EventBus)
	}
	if runtime.Sessions.IdleTimeout != "1m" {
		t.Fatalf("idle_timeout = %q, want 1m", runtime.Sessions.IdleTimeout)
	}
}

func TestLoadLayersMergeAgentPolicy(t *testing.T) {
	layers := []deploy.Layer{
		{
			Priority: 0,
			Name:     "base",
			Source: inlineSource(`version: v1
resources: {}
agents:
  tom:
    card: {name: Tom}
    policy:
      max_revise: 2
      artifact_channels: [draft]
`),
		},
		{
			Priority: 1,
			Name:     "project",
			Source: inlineSource(`agents:
  tom:
    policy:
      max_revise: 5
`),
		},
	}
	doc, _, err := deploy.LoadLayers(context.Background(), layers)
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	got := doc.Agents["tom"].Policy
	if got == nil {
		t.Fatal("merged agent has no policy")
	}
	if got.MaxRevise != 5 {
		t.Fatalf("max_revise = %d, want 5", got.MaxRevise)
	}
	if len(got.ArtifactChannels) != 1 || got.ArtifactChannels[0] != "draft" {
		t.Fatalf("artifact_channels = %v, want [draft]", got.ArtifactChannels)
	}
}

func TestLoadLayersPriorityOrder(t *testing.T) {
	layers := []deploy.Layer{
		{
			Priority: 5,
			Name:     "project",
			Source: inlineSource(`resources:
  fs: {kind: workspace.Workspace, impl: local, settings: {root: ./p}}
`),
		},
		{
			Priority: 0,
			Name:     "base",
			Source: inlineSource(`version: v1
resources:
  fs: {kind: workspace.Workspace, impl: local, settings: {root: /base}}
`),
		},
	}
	doc, _, err := deploy.LoadLayers(context.Background(), layers)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Root string `json:"root"`
	}
	if err := resource.DecodeSettings(&settings, doc.Resources["fs"].Settings); err != nil {
		t.Fatal(err)
	}
	if settings.Root != "./p" {
		t.Fatalf("root = %q, want ./p (higher priority merged later)", settings.Root)
	}
}

func TestLoadLayersPerLayerBaseDir(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dirA, "a.yaml"),
		[]byte("version: v1\nresources:\n  fs: {kind: workspace.Workspace, impl: local, settings: {root: /a}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dirB, "b.yaml"),
		[]byte("resources:\n  fs: {settings: {root: /b}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	layers := []deploy.Layer{
		{
			Priority: 0,
			BaseDir:  dirA,
			Source:   resource.Source{File: "a.yaml"},
		},
		{
			Priority: 1,
			BaseDir:  dirB,
			Source:   resource.Source{File: "b.yaml"},
		},
	}
	doc, _, err := deploy.LoadLayers(context.Background(), layers)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Root string `json:"root"`
	}
	if err := resource.DecodeSettings(&settings, doc.Resources["fs"].Settings); err != nil {
		t.Fatal(err)
	}
	if settings.Root != "/b" {
		t.Fatalf("root = %q, want /b", settings.Root)
	}
}

func TestLoadLayersRequiresVersionOnFirstLayer(t *testing.T) {
	layers := []deploy.Layer{
		{Priority: 0, Source: inlineSource("resources: {}\n")},
	}
	if _, _, err := deploy.LoadLayers(context.Background(), layers); err == nil {
		t.Fatal("LoadLayers unexpectedly accepted a versionless first layer")
	}
	layers = []deploy.Layer{
		{Priority: 0, Source: inlineSource("version: v1\nresources: {}\n")},
		{Priority: 1, Source: inlineSource("resources: {}\n")},
	}
	if _, _, err := deploy.LoadLayers(context.Background(), layers); err != nil {
		t.Fatalf("partial later layer rejected: %v", err)
	}
}

func TestLoadLayersEmbedSource(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml": &fstest.MapFile{
			Data: []byte("version: v1\nresources:\n  fs: {kind: workspace.Workspace, impl: local}\n"),
		},
	}
	layers := []deploy.Layer{{
		Priority: 0,
		Embed:    fsys,
		Source:   resource.Source{Embed: "deploy.yaml"},
	}}
	if _, _, err := deploy.LoadLayers(context.Background(), layers); err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
}

func TestBuildResolvesFileSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "settings.json"),
		[]byte(`{"root": "/tmp/x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := resource.NewRegistry()
	reg.MustRegister(workspaceFactory{})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"fs": {
				Kind:     "workspace.Registry",
				Impl:     "local",
				Settings: json.RawMessage(`{"file": "settings.json"}`),
			},
		},
	}
	builder := deploy.NewBuilder(
		reg,
		deploy.WithLoader(resource.NewLoader(resource.WithBaseDir(dir))),
	)
	result, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = result.Close() }()
	value, ok := result.Value("fs")
	if !ok {
		t.Fatal("fs missing")
	}
	if ws := value.(*workspaceRegistry); ws.root != "/tmp/x" {
		t.Fatalf("root = %q, want /tmp/x", ws.root)
	}
}

func TestBuildResolvesYAMLFileSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "settings.yaml"),
		[]byte("root: /tmp/y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := resource.NewRegistry()
	reg.MustRegister(workspaceFactory{})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"fs": {
				Kind:     "workspace.Registry",
				Impl:     "local",
				Settings: json.RawMessage(`{"file": "settings.yaml"}`),
			},
		},
	}
	builder := deploy.NewBuilder(
		reg,
		deploy.WithLoader(resource.NewLoader(resource.WithBaseDir(dir))),
	)
	result, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = result.Close() }()
	value, _ := result.Value("fs")
	if ws := value.(*workspaceRegistry); ws.root != "/tmp/y" {
		t.Fatalf("root = %q, want /tmp/y", ws.root)
	}
}
