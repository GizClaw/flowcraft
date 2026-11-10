package deploy

import (
	"context"
	"strings"
	"testing"
)

// testLoader registers one local fake kind beside the built-in a2a engine.
func testLoader(t *testing.T) Loader {
	t.Helper()
	return Loader{
		Kinds: BuiltinKinds().
			WithEngines(EngineKind{Name: "local", Local: true}).
			WithProtocols(a2aKind()),
	}
}

// An a2a engine needs no substrate; the document's settings carry its peer and
// the placeholder is resolved before the spec exists.
func TestLoadProxyAgent(t *testing.T) {
	t.Setenv("TOKEN", "s3cr3t")
	specs, err := testLoader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: translator
    engine:
      kind: a2a
      settings:
        peer: https://peer.example/.well-known/agent-card.json
        auth: {type: bearer, token: "${env:TOKEN}"}
    protocol:
      kind: a2a
      settings: {card: {version: "1.0.0", url: https://peer.example}}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d", len(specs))
	}
	spec := specs[0]
	if spec.Engine.Kind != "a2a" {
		t.Errorf("engine = %+v", spec.Engine)
	}
	if !strings.Contains(string(spec.Engine.Settings), "s3cr3t") {
		t.Errorf("engine settings = %s, want the resolved secret", spec.Engine.Settings)
	}
	if spec.Protocol == nil || spec.Protocol.Kind != "a2a" {
		t.Errorf("protocol = %+v", spec.Protocol)
	}
	if spec.Runtime.Version != 0 {
		t.Errorf("a remote engine must have no runtime, got version %d", spec.Runtime.Version)
	}
}

// A local engine needs a substrate, and the document's layers are merged.
func TestLoadLocalAgentMergesLayers(t *testing.T) {
	specs, err := testLoader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: local
    engine: {kind: local}
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

// The kind decides whether a substrate is expected; a mismatch is refused.
func TestLoadRejectsTheMatrixViolations(t *testing.T) {
	cases := map[string]string{
		"a remote engine with a runtime": `
version: 1
agents:
  - name: x
    engine: {kind: a2a, settings: {peer: https://peer.example}}
    runtime: {layers: [{priority: 0, path: ./r.yaml}]}
`,
		"a local engine without a runtime": `
version: 1
agents:
  - name: x
    engine: {kind: local}
`,
		"an engine without a peer": `
version: 1
agents:
  - name: x
    engine: {kind: a2a}
`,
	}
	for name, body := range cases {
		if _, err := testLoader(t).Load(context.Background(), []byte(body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestLoadRejectsUnknownKinds(t *testing.T) {
	_, err := testLoader(t).Load(context.Background(), []byte(`
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

// Settings are decoded strictly: a typo must fail the load, not be silently
// dropped on the way to the peer.
func TestLoadRejectsUnknownEngineSettings(t *testing.T) {
	_, err := testLoader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: x
    engine:
      kind: a2a
      settings: {peer: https://peer.example, bogus: 1}
`))
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("Load = %v, want a complaint about the unknown setting", err)
	}
}

func TestLoadRejectsUnknownFieldsAndDuplicates(t *testing.T) {
	_, err := testLoader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: x
    engine: {kind: a2a, settings: {peer: https://peer.example}}
    extra: 1
`))
	if err == nil {
		t.Fatal("an unknown field was accepted")
	}
	_, err = testLoader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: same
    engine: {kind: a2a, settings: {peer: https://a.example}}
  - name: same
    engine: {kind: a2a, settings: {peer: https://b.example}}
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate names = %v, want a complaint", err)
	}
}

func TestLoadRejectsVersionAndEmptyDocuments(t *testing.T) {
	if _, err := testLoader(t).Load(context.Background(), []byte(`version: 2`)); err == nil {
		t.Fatal("version 2 was accepted")
	}
	if _, err := testLoader(t).Load(context.Background(), []byte(`version: 1`)); err == nil {
		t.Fatal("an empty document was accepted")
	}
}

// A protocol is optional: an internal agent is a choice, not an error.
func TestLoadWithoutProtocol(t *testing.T) {
	specs, err := testLoader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: internal
    engine: {kind: a2a, settings: {peer: https://peer.example}}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if specs[0].Protocol != nil {
		t.Errorf("protocol = %+v, want none", specs[0].Protocol)
	}
}

// An unregistered scheme must fail the load: a literal "${secret:...}" would be
// sent as the credential.
func TestLoadRejectsAnUnregisteredScheme(t *testing.T) {
	_, err := testLoader(t).Load(context.Background(), []byte(`
version: 1
agents:
  - name: x
    engine:
      kind: a2a
      settings: {peer: "${secret:peer}"}
`))
	if err == nil {
		t.Fatal("a placeholder with no resolver was accepted")
	}
}
