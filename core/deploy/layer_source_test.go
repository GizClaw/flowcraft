package deploy_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/builtin"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/goccy/go-json"
)

// localEngine is the least an engine kind needs to be for the loader: it runs in
// this process, so the document may configure a runtime for it.
func localEngine() deploy.EngineKind {
	return deploy.EngineKind{Name: "local", Local: true}
}

// layerDocument is one agent whose runtime is the given layer stack. Each line of
// the stack is indented here so a case reads as the YAML it is.
func layerDocument(layers string) []byte {
	var stack strings.Builder
	for _, line := range strings.Split(strings.TrimRight(layers, "\n"), "\n") {
		stack.WriteString("        " + line + "\n")
	}
	return []byte(`version: 1
agents:
  - name: local
    engine: {kind: local}
    runtime:
      layers:
` + stack.String())
}

// layerConfig is what a layer holds: a runtime document that states its own name,
// so a test can tell which layer produced the final value.
func layerConfig(name string) string {
	root := `"` + pathtest.Abs("work") + `"`
	return "version: 1\n" +
		"name: " + name + "\n" +
		"sandbox: {backend: local, writes: [" + root + "]}\n" +
		"files: {roots: [" + root + "]}\n"
}

// fileLayer is the file source's written form. The path is rendered by
// encoding/json rather than by hand: YAML's double-quoted scalars treat a
// backslash as an escape, so a Windows path pasted in raw would be corrupted.
func fileLayer(path string) string {
	quoted, err := json.Marshal(path)
	if err != nil {
		panic(err)
	}
	return "- priority: 0\n  source: {kind: file, settings: {path: " + string(quoted) + "}}\n"
}

// memoryKind is the layer source a host writes when its layers are not files: it
// keeps them in a map and hands each back as config.Bytes.
func memoryKind(layers map[string]string) deploy.SourceKind {
	return deploy.SourceKind{
		Name: "memory",
		Open: func(_ context.Context, settings json.RawMessage) (deploy.Source, error) {
			var decoded struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(settings, &decoded); err != nil {
				return deploy.Source{}, err
			}
			body, found := layers[decoded.Name]
			if !found {
				return deploy.Source{}, errors.New("no layer named " + decoded.Name)
			}
			return deploy.Source{Name: decoded.Name, Body: config.Bytes([]byte(body))}, nil
		},
	}
}

// The source kind is the seam, not a special case for files: the file
// registration deploy/builtin ships is read through the same table a host
// extends.
func TestFileSourceReadsALayerFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(layerConfig("from-file")), 0o600); err != nil {
		t.Fatalf("write layer: %v", err)
	}
	kinds := builtin.Kinds().WithEngines(localEngine())
	specs, err := deploy.Loader{Kinds: kinds}.Load(context.Background(), layerDocument(fileLayer(path)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := specs[0].Runtime.Name; got != "from-file" {
		t.Errorf("runtime name = %q, want the file's layer", got)
	}
}

// A layer that never touches the filesystem: the bytes are the host's, and the
// document only names which one.
func TestHostSourceServesALayerFromMemory(t *testing.T) {
	kinds := deploy.Kinds{}.
		WithEngines(localEngine()).
		WithSources(memoryKind(map[string]string{"base": layerConfig("from-memory")}))
	specs, err := deploy.Loader{Kinds: kinds}.Load(context.Background(),
		layerDocument("- priority: 0\n  source: {kind: memory, settings: {name: base}}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := specs[0].Runtime.Name; got != "from-memory" {
		t.Errorf("runtime name = %q, want the in-memory layer", got)
	}
}

// The //go:embed case: the tree is a Go value, so the document names a member of
// it and the host owns the filesystem. config.FS is the Source that reads it.
func TestHostSourceServesALayerFromAnEmbeddedTree(t *testing.T) {
	tree := fstest.MapFS{
		"runtime/base.yaml": {Data: []byte(layerConfig("from-assets"))},
	}
	assets := deploy.SourceKind{
		Name: "assets",
		Open: func(_ context.Context, settings json.RawMessage) (deploy.Source, error) {
			var decoded struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(settings, &decoded); err != nil {
				return deploy.Source{}, err
			}
			return deploy.Source{Name: decoded.Name, Body: config.FS(tree, decoded.Name)}, nil
		},
	}
	specs, err := deploy.Loader{Kinds: deploy.Kinds{}.WithEngines(localEngine()).WithSources(assets)}.
		Load(context.Background(),
			layerDocument("- priority: 0\n  source: {kind: assets, settings: {name: runtime/base.yaml}}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := specs[0].Runtime.Name; got != "from-assets" {
		t.Errorf("runtime name = %q, want the embedded layer", got)
	}
}

// Sources are interchangeable within one stack: what decides the merged value is
// the priority, not where the bytes came from.
func TestSourcesMergeInPriorityOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(layerConfig("from-file")), 0o600); err != nil {
		t.Fatalf("write layer: %v", err)
	}
	kinds := deploy.Kinds{}.
		WithEngines(localEngine()).
		WithSources(
			sources.File(),
			memoryKind(map[string]string{"override": layerConfig("from-memory")}),
		)
	document := layerDocument(fileLayer(path) +
		"- priority: 10\n  source: {kind: memory, settings: {name: override}}\n")
	specs, err := deploy.Loader{Kinds: kinds}.Load(context.Background(), document)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := specs[0].Runtime.Name; got != "from-memory" {
		t.Errorf("runtime name = %q, want the higher-priority source to win", got)
	}
}

// An unregistered source is a document error naming what is registered, the same
// rule engines, protocols and hooks follow.
func TestLoadRejectsAnUnregisteredLayerSource(t *testing.T) {
	kinds := deploy.Kinds{}.
		WithEngines(localEngine()).
		WithSources(memoryKind(nil))
	_, err := deploy.Loader{Kinds: kinds}.Load(context.Background(),
		layerDocument("- priority: 0\n  source: {kind: assets}\n"))
	if err == nil {
		t.Fatal("an unregistered source was accepted")
	}
	for _, want := range []string{"not registered", "memory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A host that built its kind table by hand reads this line first, so an empty
// table says what to register rather than listing nothing.
func TestLoadExplainsAnEmptySourceTable(t *testing.T) {
	kinds := deploy.Kinds{}.WithEngines(localEngine())
	_, err := deploy.Loader{Kinds: kinds}.Load(context.Background(),
		layerDocument("- priority: 0\n  source: {kind: file, settings: {path: ./r.yaml}}\n"))
	if err == nil {
		t.Fatal("a document naming a source was accepted by an empty table")
	}
	if !strings.Contains(err.Error(), "deploy/builtin registers file") {
		t.Errorf("error %q does not say what to register", err)
	}
}

// The document may not leave the source out: defaulting to a file would be a
// choice the deployment made by accident.
func TestLoadRejectsALayerWithoutSource(t *testing.T) {
	kinds := deploy.Kinds{}.WithEngines(localEngine()).WithSources(sources.File())
	_, err := deploy.Loader{Kinds: kinds}.Load(context.Background(),
		layerDocument("- priority: 0\n"))
	if err == nil || !strings.Contains(err.Error(), "source is required") {
		t.Fatalf("err = %v, want a missing source", err)
	}
}

// The name a source reports is what the reader sees: a layer that fails to read
// is named the way its author knows it, not by its index in the stack.
func TestLoadNamesTheLayerThatFailed(t *testing.T) {
	broken := deploy.SourceKind{
		Name: "broken",
		Open: func(context.Context, json.RawMessage) (deploy.Source, error) {
			return deploy.Source{Name: "broken:missing", Body: failingSource{}}, nil
		},
	}
	kinds := deploy.Kinds{}.WithEngines(localEngine()).WithSources(broken)
	_, err := deploy.Loader{Kinds: kinds}.Load(context.Background(),
		layerDocument("- priority: 0\n  source: {kind: broken}\n"))
	if err == nil {
		t.Fatal("a layer that cannot be read was accepted")
	}
	for _, want := range []string{"broken:missing", "no such secret"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A registration that produces half a layer is a host bug, and it is reported
// where the layer was declared instead of merging as a silent no-op.
func TestLoadRejectsABrokenRegistration(t *testing.T) {
	cases := map[string]deploy.SourceKind{
		"no Open": {Name: "broken"},
		"no body": {Name: "broken", Open: func(context.Context, json.RawMessage) (deploy.Source, error) {
			return deploy.Source{Name: "x"}, nil
		}},
		"no name": {Name: "broken", Open: func(context.Context, json.RawMessage) (deploy.Source, error) {
			return deploy.Source{Body: config.Bytes(nil)}, nil
		}},
		"bad settings": {Name: "broken", Open: func(_ context.Context, settings json.RawMessage) (deploy.Source, error) {
			return deploy.Source{}, errors.New("settings.file is required")
		}},
	}
	for name, kind := range cases {
		kinds := deploy.Kinds{}.WithEngines(localEngine()).WithSources(kind)
		_, err := deploy.Loader{Kinds: kinds}.Load(context.Background(),
			layerDocument("- priority: 0\n  source: {kind: broken}\n"))
		if err == nil {
			t.Errorf("%s: a broken registration was accepted", name)
		}
	}
}

// The deployment document itself is read through the same table: a host that
// compiled its configuration into the binary names a source instead of holding
// the bytes, and LoadSource is the entry that does it.
func TestLoadSourceReadsTheDeploymentFromMemory(t *testing.T) {
	kinds := deploy.Kinds{}.
		WithEngines(deploy.EngineKind{Name: "peer"}).
		WithSources(memoryKind(map[string]string{"deploy": `version: 1
agents:
  - name: translator
    engine: {kind: peer}
`}))
	specs, err := deploy.Loader{Kinds: kinds}.LoadSource(context.Background(),
		&deploy.SourceDocument{Kind: "memory", Settings: json.RawMessage(`{"name":"deploy"}`)})
	if err != nil {
		t.Fatalf("LoadSource: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "translator" {
		t.Fatalf("specs = %+v, want the document's agent", specs)
	}
	if specs[0].Engine.Kind != "peer" {
		t.Errorf("engine = %+v, want the decoded document", specs[0].Engine)
	}
}

// LoadFile is the file source written as a call, not a second rule about where a
// document lives: a host that registered nothing is told what to register
// instead of getting a bare filesystem error.
func TestLoadFileNeedsTheFileSource(t *testing.T) {
	_, err := deploy.Loader{Kinds: deploy.Kinds{}.WithEngines(localEngine())}.
		LoadFile(context.Background(), "deploy.yaml")
	if err == nil || !strings.Contains(err.Error(), "deploy/builtin registers file") {
		t.Fatalf("err = %v, want the missing file source named", err)
	}
}

// The removed field is refused with the replacement, not as an unknown field:
// this is the message every existing deployment reads once.
func TestLoadRejectsTheRemovedPathField(t *testing.T) {
	kinds := deploy.Kinds{}.WithEngines(localEngine()).WithSources(sources.File())
	_, err := deploy.Loader{Kinds: kinds}.Load(context.Background(),
		layerDocument("- priority: 0\n  path: ./runtime.yaml\n"))
	if err == nil {
		t.Fatal("the removed layers[].path was accepted")
	}
	for _, want := range []string{"path was replaced by source", "{kind: file, settings: {path:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Layer names are how a merge failure points at its layer, so two sources that
// report one name are refused where they are declared.
func TestLoadRejectsTwoLayersWithOneName(t *testing.T) {
	kinds := deploy.Kinds{}.WithEngines(localEngine()).WithSources(sources.Inline())
	stack := "- priority: 0\n" +
		"  source: {kind: inline, settings: {body: \"version: 1\\n\"}}\n" +
		"- priority: 10\n" +
		"  source: {kind: inline, settings: {body: \"{\\\"version\\\": 1}\"}}\n"
	_, err := deploy.Loader{Kinds: kinds}.Load(context.Background(), layerDocument(stack))
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("err = %v, want the duplicate name refused", err)
	}
}

// An inline layer is the one source whose bytes live inside the deploying
// document, so the whole path — the document decode, the settings decode, the
// merge — has to survive it. The document is written as JSON because config reads
// it as YAML anyway, and a YAML string inside a YAML string is what this spelling
// avoids.
func TestInlineLayerLoadsThroughTheDocument(t *testing.T) {
	document, err := json.Marshal(map[string]any{
		"version": 1,
		"agents": []any{map[string]any{
			"name":   "local",
			"engine": map[string]any{"kind": "local"},
			"runtime": map[string]any{"layers": []any{
				map[string]any{"priority": 0, "source": map[string]any{
					"kind":     "inline",
					"settings": map[string]any{"body": layerConfig("from-inline")},
				}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	kinds := deploy.Kinds{}.WithEngines(localEngine()).WithSources(sources.Inline())
	specs, err := deploy.Loader{Kinds: kinds}.Load(context.Background(), document)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := specs[0].Runtime.Name; got != "from-inline" {
		t.Errorf("runtime name = %q, want the inline layer", got)
	}
}

// builtin registers the sources that need nothing from the host; losing one would
// make every stack that names it a load error.
func TestBuiltinRegistersTheStandardSources(t *testing.T) {
	table := builtin.Kinds().Sources
	for _, want := range []string{"file", "inline"} {
		source, found := table[want]
		if !found || source.Open == nil {
			t.Errorf("builtin does not register a working %q source: %+v", want, source)
		}
	}
}

type failingSource struct{}

func (failingSource) Body(context.Context) ([]byte, error) {
	return nil, errors.New("no such secret")
}
