package config

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"go.yaml.in/yaml/v3"
)

// benchConfig is the shape the benchmarks merge into: one flat mapping of many
// keys, one sequence, and the two scalars every document has. A real deployment
// is exactly this — sections of small mappings — so the numbers follow the
// algorithm rather than a fixture's quirks.
type benchConfig struct {
	Version int               `json:"version"`
	Name    string            `json:"name"`
	Extra   map[string]string `json:"extra"`
	Files   struct {
		Roots []string `json:"roots"`
	} `json:"files"`
}

// benchLayer writes one layer: n keys in extra, and a name that differs per
// layer so an override actually replaces something.
func benchLayer(priority int, name string, n int) Layer {
	var body strings.Builder
	body.WriteString("version: 1\n")
	body.WriteString("name: " + name + "\n")
	body.WriteString("files: {roots: [/work]}\n")
	if n > 0 {
		body.WriteString("extra:\n")
		for i := 0; i < n; i++ {
			body.WriteString("  key")
			body.WriteString(itoa(i))
			body.WriteString(": value")
			body.WriteString(itoa(i))
			body.WriteString("\n")
		}
	}
	return Layer{Priority: priority, Name: name, Source: Bytes([]byte(body.String()))}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [8]byte
	pos := len(digits)
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[pos:])
}

// BenchmarkLoad is the cost a deployment pays once per agent at startup: parse
// every layer, merge them, expand placeholders, canonicalize and decode. The
// interesting axis is not the absolute number but how it grows with the number
// of layers and the number of keys in one mapping.
func BenchmarkLoad(b *testing.B) {
	shapes := []struct {
		name       string
		layers     int
		keys       int
		provenance bool
	}{
		{name: "one-layer-16-keys", layers: 1, keys: 16, provenance: true},
		{name: "two-layers-16-keys", layers: 2, keys: 16, provenance: true},
		{name: "four-layers-16-keys", layers: 4, keys: 16, provenance: true},
		{name: "four-layers-64-keys", layers: 4, keys: 64, provenance: true},
		{name: "four-layers-256-keys", layers: 4, keys: 256, provenance: true},
		// The shape a host uses: the value is all it wants, so the merge keeps
		// no origins. The delta against the row above is what provenance costs.
		{name: "four-layers-256-keys-no-provenance", layers: 4, keys: 256},
	}
	for _, shape := range shapes {
		layers := make([]Layer, 0, shape.layers)
		for i := 0; i < shape.layers; i++ {
			layers = append(layers, benchLayer(i, "layer"+itoa(i), shape.keys))
		}
		options := []Option{}
		if !shape.provenance {
			options = append(options, WithoutProvenance())
		}
		b.Run(shape.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := Load[benchConfig](context.Background(), layers, Strict[benchConfig], options...); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLoadPlaceholders measures what a placeholder costs: one scan per
// string value, and one resolver call per reference, after the merge.
func BenchmarkLoadPlaceholders(b *testing.B) {
	body := `
version: 1
name: ${BENCH_NAME}
files: {roots: ["${BENCH_ROOT}"]}
extra:
  a: ${BENCH_VALUE}
  b: ${BENCH_VALUE:-fallback}
  c: literal
`
	b.Setenv("BENCH_NAME", "bench")
	b.Setenv("BENCH_ROOT", pathtest.Abs("work"))
	b.Setenv("BENCH_VALUE", "resolved")
	layers := []Layer{{Priority: 0, Name: "a.yaml", Source: Bytes([]byte(body))}}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := Load[benchConfig](context.Background(), layers, Strict[benchConfig]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCanonical is what a document fingerprint costs: re-encode the merged
// tree, then convert it to canonical JSON with the duplicate-key and key-spelling
// checks. It is the expensive half of a load, which is why a caller that only
// needs the value should not ask for the bytes.
func BenchmarkCanonical(b *testing.B) {
	_, doc, err := Load[benchConfig](context.Background(),
		[]Layer{benchLayer(0, "base", 16), benchLayer(10, "local", 16)}, Strict[benchConfig])
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := doc.Canonical(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRaw is a lookup by path. Each segment is a linear scan of one
// mapping's keys, so a report that reads many values should read them once: the
// number is here to make that cost visible rather than to invite a cache.
func BenchmarkRaw(b *testing.B) {
	_, doc, err := Load[benchConfig](context.Background(),
		[]Layer{benchLayer(0, "base", 256)}, Strict[benchConfig])
	if err != nil {
		b.Fatal(err)
	}
	paths := map[string][]string{
		"scalar":    {"name"},
		"first-key": {"extra", "key0"},
		"last-key":  {"extra", "key255"},
		"missing":   {"extra", "absent"},
	}
	for name, path := range paths {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = doc.Raw(path...)
			}
		})
	}
}

// BenchmarkOrigin is the provenance query a report makes per value: one map
// lookup, regardless of how many layers were merged.
func BenchmarkOrigin(b *testing.B) {
	_, doc, err := Load[benchConfig](context.Background(),
		[]Layer{benchLayer(0, "base", 64), benchLayer(10, "local", 64)}, Strict[benchConfig])
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := doc.Origin("extra", "key63"); !ok {
			b.Fatal("key63 has no origin")
		}
	}
}

// BenchmarkRequiredPlaceholders is the startup checklist: every layer is parsed
// and scanned for references without resolving any of them, so a host can name
// every missing variable before it loads anything.
func BenchmarkRequiredPlaceholders(b *testing.B) {
	layers := []Layer{
		benchLayer(0, "base", 16),
		{Priority: 10, Name: "local.yaml", Source: Bytes([]byte(`
name: ${BENCH_NAME}
files: {roots: ["${BENCH_ROOT}"]}
extra: {a: "${BENCH_A}", b: "${BENCH_B:-fallback}"}
`))},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := RequiredPlaceholders(context.Background(), layers...); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkExpandDocument expands one document without merging: the path a
// deployment document and a graph document take.
func BenchmarkExpandDocument(b *testing.B) {
	var body strings.Builder
	body.WriteString("name: ${BENCH_NAME}\n")
	for i := 0; i < 16; i++ {
		body.WriteString("key")
		body.WriteString(itoa(i))
		body.WriteString(": ${BENCH_NAME}\n")
	}
	document := []byte(body.String())
	b.Setenv("BENCH_NAME", "bench")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ExpandDocument(context.Background(), document); err != nil {
			b.Fatal(err)
		}
	}
}

// benchBodies is the fixture the stage benchmark reuses: four layers of n keys.
func benchBodies(n int) [][]byte {
	bodies := make([][]byte, 0, 4)
	for i := 0; i < 4; i++ {
		bodies = append(bodies, []byte(benchLayer(0, "layer"+itoa(i), n).Source.(bytesSource)))
	}
	return bodies
}

// BenchmarkStages splits Load into the costs it is made of, because a single
// number cannot say which stage a regression came from. Merge mutates the base
// tree, so the stages are cumulative and the deltas are the per-stage cost:
// parse, then merge, then canonicalize, then decode.
func BenchmarkStages(b *testing.B) {
	bodies := benchBodies(256)

	b.Run("parse", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, body := range bodies {
				if _, err := parseDocument(body); err != nil {
					b.Fatal(err)
				}
			}
		}
	})

	b.Run("parse+merge", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var merged *yaml.Node
			prov := Provenance{}
			for i, body := range bodies {
				document, err := parseDocument(body)
				if err != nil {
					b.Fatal(err)
				}
				ref := Ref{Priority: i, Name: "layer" + itoa(i)}
				if merged == nil {
					merged = document
					recordAll(&prov, merged, nil, ref)
					continue
				}
				merged = mergeNode(merged, document, &prov, ref, nil)
			}
		}
	})

	b.Run("parse+merge+canonical", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var merged *yaml.Node
			prov := Provenance{}
			for i, body := range bodies {
				document, err := parseDocument(body)
				if err != nil {
					b.Fatal(err)
				}
				ref := Ref{Priority: i, Name: "layer" + itoa(i)}
				if merged == nil {
					merged = document
					recordAll(&prov, merged, nil, ref)
					continue
				}
				merged = mergeNode(merged, document, &prov, ref, nil)
			}
			doc := &Doc{label: "bench", root: documentBody(merged), prov: prov}
			if _, err := doc.Canonical(); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("parse+merge+canonical+decode", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var merged *yaml.Node
			prov := Provenance{}
			for i, body := range bodies {
				document, err := parseDocument(body)
				if err != nil {
					b.Fatal(err)
				}
				ref := Ref{Priority: i, Name: "layer" + itoa(i)}
				if merged == nil {
					merged = document
					recordAll(&prov, merged, nil, ref)
					continue
				}
				merged = mergeNode(merged, document, &prov, ref, nil)
			}
			doc := &Doc{label: "bench", root: documentBody(merged), prov: prov}
			canonical, err := doc.Canonical()
			if err != nil {
				b.Fatal(err)
			}
			if _, err := Strict[benchConfig](b.Context(), canonical); err != nil {
				b.Fatal(err)
			}
		}
	})
}
