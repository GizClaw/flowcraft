package deploy_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy"
)

const yamlDoc = `version: v1
resources:
  fs:
    kind: workspace.Registry
    impl: local
    settings:
      root: /tmp/flowcraft
  box:
    kind: sandbox.Registry
    impl: bwrap
    deps:
      workspace: fs
  kit:
    kind: tool.Assembly
    impl: yaml
    deps:
      sandbox: box/coding
agents:
  researcher:
    card:
      name: Researcher
      description: Does research
    tools: [search]
    engine:
      kind: graph
      deps:
        workspace: fs
`

func TestParseYAML(t *testing.T) {
	doc, err := deploy.Parse([]byte(yamlDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Version != "v1" {
		t.Fatalf("Version = %q", doc.Version)
	}
	fs, ok := doc.Resources["fs"]
	if !ok || fs.Kind != "workspace.Registry" || fs.Impl != "local" {
		t.Fatalf("fs resource = %+v", fs)
	}
	if got := string(fs.Settings); !strings.Contains(got, "/tmp/flowcraft") {
		t.Fatalf("fs settings = %s", got)
	}
	box := doc.Resources["box"]
	if box.Deps["workspace"] != "fs" {
		t.Fatalf("box deps = %v", box.Deps)
	}
	kit := doc.Resources["kit"]
	if kit.Deps["sandbox"] != "box/coding" {
		t.Fatalf("kit deps = %v", kit.Deps)
	}
	agent, ok := doc.Agents["researcher"]
	if !ok || agent.Card.Name != "Researcher" || agent.Engine.Kind != "graph" {
		t.Fatalf("researcher agent = %+v", agent)
	}
	if len(agent.Tools) != 1 || agent.Tools[0] != "search" {
		t.Fatalf("agent tools = %v", agent.Tools)
	}
}

func TestParseJSON(t *testing.T) {
	doc, err := deploy.Parse([]byte(`{
		"version": "v1",
		"resources": {"fs": {"kind": "workspace.Registry", "impl": "mem"}}
	}`))
	if err != nil {
		t.Fatalf("Parse JSON: %v", err)
	}
	if doc.Resources["fs"].Impl != "mem" {
		t.Fatalf("fs impl = %q", doc.Resources["fs"].Impl)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	_, err := deploy.Parse([]byte("version: v1\nbogus: 1\nresources: {}\n"))
	if err == nil {
		t.Fatal("Parse unexpectedly accepted unknown field")
	}
}

func TestParseRejectsMissingVersion(t *testing.T) {
	_, err := deploy.Parse([]byte("resources: {}\n"))
	if err == nil {
		t.Fatal("Parse unexpectedly accepted missing version")
	}
}

func TestParseRejectsInvalidAgent(t *testing.T) {
	_, err := deploy.Parse([]byte(`version: v1
resources: {}
agents:
  x:
    engine: {kind: graph}
`))
	if err == nil {
		t.Fatal("Parse unexpectedly accepted agent without card name")
	}
}

func TestParseRejectsMultipleDocuments(t *testing.T) {
	_, err := deploy.Parse([]byte("version: v1\nresources: {}\n---\nversion: v2\n"))
	if err == nil {
		t.Fatal("Parse unexpectedly accepted multiple YAML documents")
	}
}
