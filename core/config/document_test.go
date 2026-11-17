package config

import (
	"context"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

// Raw answers "what did the merge produce here" for a host that reports its
// effective configuration, without a second decode into a type per section.
func TestDocRawReadsTheMergedTree(t *testing.T) {
	_, doc := mustLoad(t, base(), override(platformPaths(`
sandbox:
  writes: [/work, /tmp]
tools:
  tty: true
`)))
	value, ok := doc.Raw("sandbox", "writes")
	if !ok {
		t.Fatal("sandbox.writes is missing")
	}
	writes, ok := value.([]any)
	if !ok || len(writes) != 2 {
		t.Fatalf("sandbox.writes = %#v, want the override's two entries", value)
	}
	if got, ok := doc.Raw("name"); !ok || got != "forge" {
		t.Errorf("name = %#v, %v", got, ok)
	}
	if got, ok := doc.Raw("tools", "tty"); !ok || got != true {
		t.Errorf("tools.tty = %#v, %v", got, ok)
	}
	// A path the document does not have, and a path that walks through a
	// scalar, are both absent rather than an error: a report asks about
	// sections a deployment may not use.
	if _, ok := doc.Raw("sandbox", "absent"); ok {
		t.Error("an absent key was reported as present")
	}
	if _, ok := doc.Raw("sandbox", "backend", "deeper"); ok {
		t.Error("a path through a scalar was reported as present")
	}
}

// Raw reports the expanded value, not the reference that was written: it is the
// merged document the value was decoded from.
func TestDocRawSeesResolvedPlaceholders(t *testing.T) {
	t.Setenv("CONFIG_TEST_NAME", "forge")
	_, doc := mustLoad(t, Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(
		"version: 1\nname: ${CONFIG_TEST_NAME}\nsandbox: {backend: local}\nfiles: {roots: [/work]}\n",
	))})
	if got, ok := doc.Raw("name"); !ok || got != "forge" {
		t.Errorf("name = %#v, %v want the resolved value", got, ok)
	}
}

// Canonical is the document's identity: two stacks that differ only in
// formatting, key order or which file a value came from produce the same bytes.
func TestCanonicalIgnoresFormattingAndLayerSplit(t *testing.T) {
	one, doc := mustLoad(t,
		Layer{Priority: 0, Name: "a.yaml", Source: Bytes([]byte("version: 1\nname: forge\n"))},
		Layer{Priority: 10, Name: "b.yaml", Source: Bytes([]byte("sandbox: {backend: local}\nfiles: {roots: [/work]}\n"))},
	)
	two, other := mustLoad(t,
		Layer{Priority: 0, Name: "whole.yaml", Source: Bytes([]byte(`
files:
  roots: [/work]
sandbox:
  backend: local
name: forge
version: 1
`))},
	)
	if one.Version != 1 || two.Version != 1 || one.Sandbox.Backend != two.Sandbox.Backend {
		fatal := one.Sandbox.Backend
		t.Fatalf("the two stacks decode differently: %q and %q", fatal, two.Sandbox.Backend)
	}
	first, err := doc.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	second, err := other.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("canonical forms differ:\n%s\n%s", first, second)
	}
}

func TestDocLabel(t *testing.T) {
	_, doc, err := Load[testConfig](context.Background(),
		[]Layer{{Priority: 0, Name: "a.yaml", Source: Bytes([]byte("version: 1\n"))}},
		Strict[testConfig], WithLabel("deploy"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if doc.Label() != "deploy" {
		t.Errorf("label = %q", doc.Label())
	}
}

// Provenance is reported in supply order, so a report prints the same way on
// every run; Len counts values, not layers.
func TestProvenanceEachIsOrdered(t *testing.T) {
	_, doc := mustLoad(t, base(), override("tools: {tty: true}\n"))
	prov := doc.Provenance()
	if prov.Len() == 0 {
		t.Fatal("nothing was recorded")
	}
	var paths []string
	prov.Each(func(path []string, ref Ref) bool {
		paths = append(paths, strings.Join(path, ".")+"="+ref.Name)
		return true
	})
	if len(paths) != prov.Len() {
		t.Fatalf("Each visited %d of %d", len(paths), prov.Len())
	}
	if paths[0] != "version=base.yaml" {
		t.Errorf("first recorded value = %q, want the base layer's first key", paths[0])
	}
	// A key that contains a dot keeps its meaning: paths are compared as
	// segments, not as a joined string.
	if _, ok := prov.Origin("tools", "tty"); !ok {
		t.Error("tools.tty was not recorded")
	}
}

// The decode seam exists so a caller can choose different semantics; Strict is
// only the default. This decoder accepts what Strict refuses.
func TestLoadWithACustomDecoder(t *testing.T) {
	layer := Layer{Priority: 0, Name: "a.yaml", Source: Bytes([]byte("name: forge\nunknown: 1\n"))}
	if _, _, err := load(t, layer); err == nil {
		t.Fatal("Strict accepted an unknown field")
	}

	lenient := func(_ context.Context, canonical []byte) (testConfig, error) {
		var value testConfig
		err := json.Unmarshal(canonical, &value)
		return value, err
	}
	cfg, _, err := Load[testConfig](context.Background(), []Layer{layer}, lenient)
	if err != nil {
		t.Fatalf("Load with a lenient decoder: %v", err)
	}
	if cfg.Name != "forge" {
		t.Errorf("name = %q", cfg.Name)
	}
}

// Decode is the single-document path: a deployment document is expanded by the
// same rules a layer is, then decoded.
func TestDecodeExpandsAndDecodesOneDocument(t *testing.T) {
	t.Setenv("CONFIG_TEST_PEER", "https://peer.example")
	cfg, err := Decode[testConfig](context.Background(),
		[]byte("version: 1\nname: ${CONFIG_TEST_PEER}\n"),
		Strict[testConfig])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.Name != "https://peer.example" {
		t.Errorf("name = %q", cfg.Name)
	}
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	_, err := Decode[testConfig](context.Background(), []byte("nope: 1\n"), Strict[testConfig])
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want the unknown field named", err)
	}
}

// The single-document path applies the same rule: one document, expanded, then
// decoded.
func TestDecodeRejectsMoreThanOneDocument(t *testing.T) {
	_, err := Decode[testConfig](context.Background(),
		[]byte("version: 1\n---\nversion: 2\n"), Strict[testConfig])
	if err == nil {
		t.Fatal("a two-document body was accepted")
	}
}
