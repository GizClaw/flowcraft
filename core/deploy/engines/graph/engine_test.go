package graph_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/builtin"
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

// fileSource is the document reference in each of its forms: a path the file
// source reads, and the same thing spelled out.
func fileSource(path string) map[string]any {
	return map[string]any{"kind": "file", "settings": map[string]any{"path": path}}
}

// table is the source table these tests resolve through: the file registration
// deploy/builtin ships, plus whatever a case adds.
func table(t *testing.T) deploy.Kinds {
	t.Helper()
	return builtin.Kinds()
}

func opener(t *testing.T, kinds deploy.Kinds) deploy.SourceOpener {
	t.Helper()
	return func(ctx context.Context, document *deploy.LayerSourceDocument) (deploy.LayerSource, error) {
		return kinds.OpenSource(ctx, "engine settings", document)
	}
}

func decode(t *testing.T, kinds deploy.Kinds, raw json.RawMessage) (any, error) {
	t.Helper()
	return graph.Engine().Decode(context.Background(), raw, opener(t, kinds))
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
func TestDecodeRequiresASource(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"absent":  nil,
		"null":    json.RawMessage(`null`),
		"empty":   json.RawMessage(`{}`),
		"no kind": settings(t, map[string]any{"source": map[string]any{"settings": map[string]any{"path": doc}}}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decode(t, table(t), raw); err == nil ||
				!strings.Contains(err.Error(), "settings.source") {
				t.Fatalf("Decode = %v, want a missing-source complaint", err)
			}
		})
	}
}

// An unregistered source is a load error naming what the host did register, the
// same rule a runtime layer follows.
func TestDecodeRejectsAnUnregisteredSource(t *testing.T) {
	raw := settings(t, map[string]any{"source": map[string]any{"kind": "vault"}})
	_, err := decode(t, table(t), raw)
	if err == nil {
		t.Fatal("an unregistered source was accepted")
	}
	for _, want := range []string{"not registered", "file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
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
	raw := settings(t, map[string]any{"source": fileSource(write(t, "not: a graph\n"))})
	if _, err := decode(t, table(t), raw); err == nil {
		t.Fatal("an unparsable graph document was accepted")
	}
}

// Settings are decoded strictly: a typo must not be dropped on the way to the
// compiler. The source's own settings are decoded by the source, so a typo there
// is caught by the same rule one level down.
func TestDecodeRejectsUnknownSettings(t *testing.T) {
	cases := map[string]map[string]any{
		"beside the source": {"source": fileSource(write(t, doc)), "bogus": 1},
		"inside the source": {"source": map[string]any{
			"kind":     "file",
			"settings": map[string]any{"path": write(t, doc), "bogus": 1},
		}},
	}
	for name, value := range cases {
		_, err := decode(t, table(t), settings(t, value))
		if err == nil || !strings.Contains(err.Error(), "bogus") {
			t.Errorf("%s: Decode = %v, want a complaint about the unknown setting", name, err)
		}
	}
}

// The point of the source table: a document compiled into the host runs the
// engine without a file existing anywhere.
func TestDocumentComesFromTheSourceTable(t *testing.T) {
	kinds := deploy.Kinds{}.WithLayerSources(deploy.LayerSourceKind{
		Name: "memory",
		Open: func(_ context.Context, settings json.RawMessage) (deploy.LayerSource, error) {
			var decoded struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(settings, &decoded); err != nil {
				return deploy.LayerSource{}, err
			}
			if decoded.Name != "graph" {
				return deploy.LayerSource{}, errors.New("no document named " + decoded.Name)
			}
			return deploy.LayerSource{Name: "memory:graph", Body: config.Bytes([]byte(doc))}, nil
		},
	})
	raw := settings(t, map[string]any{"source": map[string]any{"kind": "memory", "settings": map[string]any{"name": "graph"}}})
	if _, err := decode(t, kinds, raw); err != nil {
		t.Fatalf("Decode: %v", err)
	}
}

// The fingerprint is the document's identity: editing the file must invalidate
// the checkpoints taken under the old graph.
func TestFingerprintTracksTheDocumentBytes(t *testing.T) {
	kinds := table(t)
	kind := graph.Engine()
	path := write(t, doc)
	raw := settings(t, map[string]any{"source": fileSource(path)})

	before := kind.Fingerprint(context.Background(), raw, opener(t, kinds))
	if again := kind.Fingerprint(context.Background(), raw, opener(t, kinds)); again != before {
		t.Error("the same document produced two fingerprints")
	}
	if err := os.WriteFile(path, []byte(doc+"\n# edited\n"), 0o600); err != nil {
		t.Fatalf("edit graph: %v", err)
	}
	if after := kind.Fingerprint(context.Background(), raw, opener(t, kinds)); after == before {
		t.Error("editing the document did not change the fingerprint")
	}
}

// Two documents with the same bytes under different names are different
// deployments: the name is part of the identity the checkpoint records.
func TestFingerprintTracksTheSourceName(t *testing.T) {
	kinds := table(t)
	kind := graph.Engine()
	one := kind.Fingerprint(context.Background(),
		settings(t, map[string]any{"source": fileSource(write(t, doc))}), opener(t, kinds))
	two := kind.Fingerprint(context.Background(),
		settings(t, map[string]any{"source": fileSource(write(t, doc))}), opener(t, kinds))
	if one == two {
		t.Error("the same bytes under two names produced one fingerprint")
	}
}

// A host that registered nothing gets a fingerprint that matches no deployment,
// rather than one that silently equals an empty document's.
func TestFingerprintOfAnUnreadableSourceIsInvalid(t *testing.T) {
	raw := settings(t, map[string]any{"source": map[string]any{"kind": "absent"}})
	if got := graph.Engine().Fingerprint(context.Background(), raw, opener(t, deploy.Kinds{})); got != "invalid" {
		t.Errorf("fingerprint = %q, want invalid", got)
	}
}
