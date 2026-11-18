package graph_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy/engines/graph"
	"github.com/goccy/go-json"
)

const doc = `version: 1
entry: a
state:
  text: {type: text}
nodes:
  - {id: a, kind: fixed}
edges:
  - {from: a, to: end}
output: {key: text}
`

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write graph: %v", err)
	}
	return path
}

func settings(t *testing.T, value any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	return body
}

func TestEngineIsALocalGraph(t *testing.T) {
	kind := graph.Engine()
	if kind.Name != "graph" {
		t.Errorf("name = %q, want graph", kind.Name)
	}
	if !kind.Local {
		t.Error("the graph engine must be local: it needs a runtime")
	}
}

// A graph without a document is a wiring mistake, and it fails at load.
func TestDecodeRequiresAPath(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"absent": nil,
		"null":   json.RawMessage(`null`),
		"empty":  settings(t, map[string]any{"path": "  "}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := graph.Engine().Decode(raw); err == nil || !strings.Contains(err.Error(), "settings.path") {
				t.Fatalf("Decode = %v, want a missing-path complaint", err)
			}
		})
	}
}

// A document that will not parse is refused at load, not when the first task
// starts.
//
// What the probe does not do is check the wiring: nodes.Validate reads and parses
// the document and stops there, so a version this runtime does not support, an
// entry that names no node, or an empty node list all load and fail at Build
// instead. The document is still worth validating here because a syntax error is
// the most common wiring mistake and it costs one read.
func TestDecodeRejectsADocumentThatDoesNotParse(t *testing.T) {
	_, err := graph.Engine().Decode(settings(t, map[string]any{"path": write(t, "not: a graph\n")}))
	if err == nil {
		t.Fatal("an unparsable graph document was accepted")
	}
}

// Settings are decoded strictly: a typo must not be dropped on the way to the
// compiler.
func TestDecodeRejectsUnknownSettings(t *testing.T) {
	raw := settings(t, map[string]any{"path": write(t, doc), "bogus": 1})
	_, err := graph.Engine().Decode(raw)
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("Decode = %v, want a complaint about the unknown setting", err)
	}
}

// The fingerprint is the document's identity: editing the file must invalidate
// the checkpoints taken under the old graph.
func TestFingerprintTracksTheDocumentBytes(t *testing.T) {
	path := write(t, doc)
	fingerprint := graph.Engine().Fingerprint
	before := fingerprint(settings(t, map[string]any{"path": path}))
	if again := fingerprint(settings(t, map[string]any{"path": path})); again != before {
		t.Error("the same document produced two fingerprints")
	}
	if err := os.WriteFile(path, []byte(doc+"\n# edited\n"), 0o600); err != nil {
		t.Fatalf("edit graph: %v", err)
	}
	if after := fingerprint(settings(t, map[string]any{"path": path})); after == before {
		t.Error("editing the document did not change the fingerprint")
	}
}
