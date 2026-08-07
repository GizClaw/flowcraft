package app

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

func TestAbsolutizeDeploymentRewritesRelativeFilesAsJSON(t *testing.T) {
	raw := []byte(`
version: v1
agents: {}
resources:
  workspace:
    kind: workspace.Registry
    impl: yaml
    settings: {file: workspace.yaml}
  inference:
    kind: inference.Assembly
    impl: yaml
    settings: {file: /abs/inference.yaml}
  inline:
    kind: workspace.Registry
    impl: yaml
    settings:
      inline:
        version: v1
        workspaces: {}
`)
	out, err := absolutizeDeployment(raw, "/scenario")
	if err != nil {
		t.Fatalf("absolutizeDeployment: %v", err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(out), []byte("{")) {
		t.Fatalf("output is not JSON: %s", out)
	}
	if _, err := deploy.Parse(out); err != nil {
		t.Fatalf("deploy.Parse(JSON): %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("json.Unmarshal(output): %v", err)
	}
	resources := doc["resources"].(map[string]any)
	workspace := resources["workspace"].(map[string]any)["settings"].(map[string]any)
	if got := workspace["file"]; got != "/scenario/workspace.yaml" {
		t.Fatalf("workspace file = %v, want /scenario/workspace.yaml", got)
	}
	inference := resources["inference"].(map[string]any)["settings"].(map[string]any)
	if got := inference["file"]; got != "/abs/inference.yaml" {
		t.Fatalf("absolute file = %v, want unchanged", got)
	}
	inline := resources["inline"].(map[string]any)["settings"].(map[string]any)
	if _, ok := inline["inline"]; !ok {
		t.Fatalf("inline settings were not preserved: %v", inline)
	}
}

func TestAbsolutizeDeploymentRejectsMultipleDocuments(t *testing.T) {
	raw := []byte("version: v1\n---\nversion: v1\n")
	if _, err := absolutizeDeployment(raw, "/x"); err == nil {
		t.Fatal("expected multiple YAML documents to be rejected")
	}
}
