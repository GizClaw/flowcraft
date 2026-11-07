package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

const runtimeLayer = `
version: 1
sandbox: {backend: local, writes: ["/work"]}
files: {roots: ["/work"]}
inference:
  providers: [{name: p, driver: stub}]
  models: [{provider: p, model: m}]
`

func loader(t *testing.T) Loader {
	t.Helper()
	return Loader{
		Engines: []Engine{{Name: "a2a"}, {Name: "graph"}},
		Protocols: []Protocol{{
			Name: "a2a",
			Decode: func(settings map[string]any) (any, error) {
				if _, ok := settings["card"]; !ok {
					return nil, errdefs.Validationf("card is required")
				}
				return settings["card"], nil
			},
		}},
	}
}

func writeLayer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(runtimeLayer), 0o600); err != nil {
		t.Fatalf("write layer: %v", err)
	}
	return path
}

// A proxy needs no substrate; a local engine cannot do without one.
func TestLoadProxyAgent(t *testing.T) {
	specs, err := loader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: translator
    engine:
      kind: a2a
      peer: https://peer.example/.well-known/agent-card.json
      auth: {type: bearer, token: "${env:TOKEN}"}
    protocol:
      kind: a2a
      settings: {card: {description: 翻译}}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d", len(specs))
	}
	spec := specs[0]
	if spec.Engine.Kind != "a2a" || spec.Engine.Peer == "" {
		t.Errorf("engine = %+v", spec.Engine)
	}
	if spec.Protocol == nil || spec.Protocol.Kind != "a2a" {
		t.Errorf("protocol = %+v", spec.Protocol)
	}
	if spec.Runtime.Version != 0 {
		t.Errorf("a proxy must have no runtime, got version %d", spec.Runtime.Version)
	}
}

func TestLoadLocalAgentMergesLayers(t *testing.T) {
	specs, err := loader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: local
    engine: {kind: graph, graph: ./g.yaml}
    runtime:
      layers:
        - {priority: 0, path: `+writeLayer(t)+`}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if specs[0].Runtime.Version != 1 {
		t.Errorf("runtime version = %d, want the merged layer", specs[0].Runtime.Version)
	}
	if len(specs[0].Runtime.Files.Roots) != 1 {
		t.Errorf("roots = %v", specs[0].Runtime.Files.Roots)
	}
}

// Protocol omitted means the agent is not exposed; that is a choice, not an
// error.
func TestLoadWithoutProtocol(t *testing.T) {
	specs, err := loader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: internal
    engine: {kind: a2a, peer: https://peer.example}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if specs[0].Protocol != nil {
		t.Errorf("protocol = %+v, want none", specs[0].Protocol)
	}
}

func TestLoadRejectsTheMatrixViolations(t *testing.T) {
	cases := map[string]string{
		"a proxy with a runtime": `
version: 1
agents:
  - name: x
    engine: {kind: a2a, peer: https://peer.example}
    runtime: {layers: [{priority: 0, path: ./r.yaml}]}
`,
		"a local engine without a runtime": `
version: 1
agents:
  - name: x
    engine: {kind: graph, graph: ./g.yaml}
`,
		"a peer on a local engine": `
version: 1
agents:
  - name: x
    engine: {kind: graph, graph: ./g.yaml, peer: https://peer.example}
    runtime: {layers: [{priority: 0, path: ./r.yaml}]}
`,
		"a proxy without a peer": `
version: 1
agents:
  - name: x
    engine: {kind: a2a}
`,
	}
	for name, body := range cases {
		if _, err := loader(t).Load(context.Background(), []byte(body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestLoadRejectsUnknownKinds(t *testing.T) {
	_, err := loader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: x
    engine: {kind: react}
`))
	if err == nil {
		t.Fatal("an unregistered engine was accepted")
	}
	if !strings.Contains(err.Error(), "a2a") {
		t.Errorf("error %q does not list the registered engines", err)
	}
}

func TestLoadRejectsUnknownFieldsAndDuplicates(t *testing.T) {
	if _, err := loader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: x
    engin: {kind: a2a}
`)); err == nil {
		t.Error("a misspelled key was accepted")
	}
	if _, err := loader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: x
    engine: {kind: a2a, peer: https://peer.example}
  - name: x
    engine: {kind: a2a, peer: https://peer.example}
`)); err == nil {
		t.Error("a duplicate agent name was accepted")
	}
}

// A protocol block that is present but unusable fails at load time, before any
// runtime, engine or surface is built.
func TestLoadValidatesProtocolSettings(t *testing.T) {
	_, err := loader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: x
    engine: {kind: a2a, peer: https://peer.example}
    protocol: {kind: a2a}
`))
	if err == nil {
		t.Fatal("a protocol without a card was accepted")
	}
	if !strings.Contains(err.Error(), "card") {
		t.Errorf("error %q does not name the missing setting", err)
	}

	specs, err := loader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: x
    engine: {kind: a2a, peer: https://peer.example}
    protocol:
      kind: a2a
      card: {description: hello, version: "1.0.0", url: https://x}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if specs[0].Protocol == nil || specs[0].Protocol.Decoded == nil {
		t.Errorf("protocol = %+v, want the decoded settings", specs[0].Protocol)
	}
}
