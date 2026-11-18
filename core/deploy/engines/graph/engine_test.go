package graph_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	return func(ctx context.Context, document *deploy.SourceDocument) (deploy.Source, error) {
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
	cases := map[string]struct {
		raw  json.RawMessage
		want string
	}{
		"absent":  {nil, "settings.source is required"},
		"null":    {json.RawMessage(`null`), "settings.source is required"},
		"empty":   {json.RawMessage(`{}`), "settings.source is required"},
		"no kind": {settings(t, map[string]any{"source": map[string]any{}}), "source.kind is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decode(t, table(t), tc.raw); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Decode = %v, want %q", err, tc.want)
			}
		})
	}
}

// The removed setting is refused with the replacement: this is the message every
// existing deployment reads once, and "unknown field path" names neither the
// location nor what to write instead.
func TestDecodeRejectsTheRemovedPathSetting(t *testing.T) {
	_, err := decode(t, table(t), settings(t, map[string]any{"path": write(t, doc)}))
	if err == nil {
		t.Fatal("the removed settings.path was accepted")
	}
	for _, want := range []string{"path was replaced by source", "settings: {path:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The half-migrated spelling inside the source block - a path copied next to
// kind instead of into the source's settings - is the most likely wrong rewrite,
// so it is refused with the replacement rather than as an unknown field.
func TestDecodeRejectsAPathInsideTheSource(t *testing.T) {
	_, err := decode(t, table(t), settings(t, map[string]any{
		"source": map[string]any{"path": write(t, doc)},
	}))
	if err == nil {
		t.Fatal("the half-migrated source.path was accepted")
	}
	if !strings.Contains(err.Error(), "source.path was replaced by source.settings.path") {
		t.Fatalf("Decode = %v, want the half-migration message", err)
	}
}

// A host that calls Decode without a table gets a sentence, not a panic.
func TestDecodeRejectsANilSourceTable(t *testing.T) {
	raw := settings(t, map[string]any{"source": fileSource(write(t, doc))})
	_, err := graph.Engine().Decode(context.Background(), raw, nil)
	if err == nil || !strings.Contains(err.Error(), "no source table") {
		t.Fatalf("Decode = %v, want the missing table named", err)
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
	kinds := deploy.Kinds{}.WithSources(deploy.SourceKind{
		Name: "memory",
		Open: func(_ context.Context, settings json.RawMessage) (deploy.Source, error) {
			var decoded struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(settings, &decoded); err != nil {
				return deploy.Source{}, err
			}
			if decoded.Name != "graph" {
				return deploy.Source{}, errors.New("no document named " + decoded.Name)
			}
			return deploy.Source{Name: "memory:graph", Body: config.Bytes([]byte(doc))}, nil
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
// rather than one that silently equals an empty document's. Two different broken
// deployments must not share one either: that would resume each other's
// checkpoints.
func TestFingerprintOfAnUnreadableSourceIsDistinct(t *testing.T) {
	kind := graph.Engine()
	one := kind.Fingerprint(context.Background(),
		settings(t, map[string]any{"source": map[string]any{"kind": "absent"}}), opener(t, deploy.Kinds{}))
	two := kind.Fingerprint(context.Background(),
		settings(t, map[string]any{"source": map[string]any{"kind": "other"}}), opener(t, deploy.Kinds{}))
	for _, got := range []string{one, two} {
		if !strings.HasPrefix(got, "invalid:") {
			t.Errorf("fingerprint = %q, want an invalid: identity", got)
		}
	}
	if one == two {
		t.Error("two broken deployments share one identity")
	}
}

// The preimage is pinned: sha256(name "\x00" bytes), which is what a file-backed
// graph hashed before this change. An upgrade that moved it would silently discard
// every in-flight task of every graph deployment.
func TestFingerprintPreimageIsPinned(t *testing.T) {
	path := write(t, doc)
	raw := settings(t, map[string]any{"source": fileSource(path)})
	got := graph.Engine().Fingerprint(context.Background(), raw, opener(t, table(t)))
	sum := sha256.Sum256([]byte(path + "\x00" + doc))
	if want := "sha256:" + hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("fingerprint = %q, want %q (the pre-change preimage)", got, want)
	}
}
