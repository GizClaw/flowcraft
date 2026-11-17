package config

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// --- what a document may be ---------------------------------------------

// A higher layer may replace a mapping with a scalar. The merge does it, and the
// decode is what reports it: the point is that the merge did not keep the old
// mapping and quietly add the scalar.
func TestLoadReplacesAMappingWithAScalar(t *testing.T) {
	_, _, err := load(t,
		base(),
		override("sandbox: 7\n"),
	)
	if err == nil {
		t.Fatal("a mapping replaced by a scalar was accepted")
	}
	if !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Errorf("err = %v, want the replacement reported by the decoder", err)
	}
}

// The reverse direction is legal and common: a scalar in the base, a mapping in
// the profile.
func TestLoadReplacesAScalarWithAMapping(t *testing.T) {
	cfg, _ := mustLoad(t,
		Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(
			"version: 1\nname: forge\nsandbox: {backend: local}\nfiles: {roots: [/work]}\n"))},
		Layer{Priority: 10, Name: "local.yaml", Source: Bytes([]byte(
			"sandbox: {backend: bwrap}\n"))},
	)
	if cfg.Sandbox.Backend != "bwrap" {
		t.Errorf("backend = %q", cfg.Sandbox.Backend)
	}
}

// A duplicate key inside an element of a sequence is a different mapping, and
// still a typo: the walk reaches it even though two elements may repeat a key.
func TestLoadRejectsDuplicateKeyInsideASequenceElement(t *testing.T) {
	_, _, err := load(t, Layer{Priority: 0, Name: "a.yaml", Source: Bytes([]byte(`
version: 1
sandbox: {backend: local}
files: {roots: [/work]}
inference:
  models:
    - provider: p
      provider: q
      model: m
`))})
	if err == nil {
		t.Fatal("a duplicate key inside a sequence element was accepted")
	}
	if !strings.Contains(err.Error(), "provider") {
		t.Errorf("err = %v, want the key named", err)
	}
}

// A trailing `---` starts a second (empty) document; YAML would stop at the
// first, so the refusal is the only way the reader learns the file was broken.
func TestLoadRejectsATrailingSeparator(t *testing.T) {
	_, _, err := load(t, Layer{Priority: 0, Name: "a.yaml", Source: Bytes([]byte(
		"version: 1\nname: forge\n---\n"))})
	if err == nil {
		t.Fatal("a trailing document separator was accepted")
	}
	if !strings.Contains(err.Error(), "more than one YAML document") {
		t.Errorf("err = %v", err)
	}
}

// A key the conversion to JSON cannot spell is refused by name: the merge would
// otherwise hand the decoder a document that lost a key.
func TestLoadRejectsAKeyTheConversionCannotSpell(t *testing.T) {
	_, _, err := load(t, Layer{Priority: 0, Name: "a.yaml", Source: Bytes([]byte(
		"version: 1\n~: 1\n"))})
	if err == nil {
		t.Fatal("a null key was accepted")
	}
	if !strings.Contains(err.Error(), "cannot be spelled") {
		t.Errorf("err = %v", err)
	}
}

// A key that is a collection has no JSON spelling either, and the YAML layer
// refuses it before the conversion can be attempted.
func TestLoadRejectsAKeyThatIsACollection(t *testing.T) {
	_, _, err := load(t, Layer{Priority: 0, Name: "a.yaml", Source: Bytes([]byte(
		"version: 1\n? [a, b]\n: 1\n"))})
	if err == nil {
		t.Fatal("a sequence key was accepted")
	}
}

// --- the single-document path -------------------------------------------

// A nil decoder means Strict: a caller does not have to spell the default.
func TestDecodeDefaultsToStrict(t *testing.T) {
	if _, _, err := Load[testConfig](context.Background(),
		[]Layer{{Priority: 0, Name: "a.yaml", Source: Bytes([]byte("unknown: 1\n"))}}, nil); err == nil {
		t.Fatal("Load with a nil decoder accepted an unknown field")
	}
	if _, err := Decode[testConfig](context.Background(), []byte("unknown: 1\n"), nil); err == nil {
		t.Fatal("Decode with a nil decoder accepted an unknown field")
	}
}

// A document that carries nothing is not a configuration with defaults.
func TestDecodeRejectsAnEmptyDocument(t *testing.T) {
	_, err := Decode[testConfig](context.Background(), []byte("# nothing here\n"), Strict[testConfig])
	if err == nil {
		t.Fatal("an empty document was accepted")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v", err)
	}
}

func TestDecodeRejectsAMalformedDocument(t *testing.T) {
	if _, err := Decode[testConfig](context.Background(), []byte("name: [unclosed\n"), Strict[testConfig]); err == nil {
		t.Fatal("a malformed document was accepted")
	}
}

// A host scheme reaches a single document the same way it reaches a layer.
func TestDecodeResolvesAHostScheme(t *testing.T) {
	cfg, err := Decode[testConfig](context.Background(),
		[]byte("version: 1\nname: ${secret:forge:name}\n"),
		Strict[testConfig],
		WithResolver("secret", func(_ context.Context, args []string) (string, error) {
			return strings.Join(args, "/"), nil
		}),
	)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.Name != "forge/name" {
		t.Errorf("name = %q", cfg.Name)
	}
}

// A document whose root is a single string has no path to name, so the error
// must name the document instead of rendering an empty location.
func TestDecodeNamesTheDocumentWhenThereIsNoPath(t *testing.T) {
	_, err := Decode[testConfig](context.Background(), []byte("${CONFIG_TEST_UNSET_PLACEHOLDER}\n"),
		Strict[testConfig], WithLabel("deploy"))
	if err == nil {
		t.Fatal("an unset reference in a scalar document was accepted")
	}
	if !strings.HasPrefix(err.Error(), "deploy: ") || strings.Contains(err.Error(), "deploy: : ") {
		t.Errorf("err = %v, want the document named without an empty path", err)
	}
}

// --- placeholders -------------------------------------------------------

// The scanner's contract, one row per form: what is a reference, what is a
// literal, and what is a malformed document.
func TestExpandDocumentForms(t *testing.T) {
	t.Setenv("CONFIG_TEST_SET", "value")
	t.Setenv("CONFIG_TEST_EMPTY", "")
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr string
	}{
		{name: "bare", value: "${CONFIG_TEST_SET}", want: "value"},
		{name: "qualified-env", value: "${env:CONFIG_TEST_SET}", want: "value"},
		{name: "set-but-empty", value: "${CONFIG_TEST_EMPTY}", want: ""},
		{name: "fallback-unused", value: "${CONFIG_TEST_SET:-other}", want: "value"},
		{name: "fallback-used", value: "${CONFIG_TEST_ABSENT:-other}", want: "other"},
		{name: "fallback-empty", value: "${CONFIG_TEST_ABSENT:-}", want: ""},
		{name: "literal-dollar", value: "$$CONFIG_TEST_SET", want: "$CONFIG_TEST_SET"},
		{name: "escaped-dollar-alone", value: "$$5", want: "$5"},
		{name: "dollar-is-not-a-reference", value: "100$ and 5", want: "100$ and 5"},
		{name: "escaped-reference", value: "$${CONFIG_TEST_SET}", want: "${CONFIG_TEST_SET}"},
		{name: "two-references", value: "${CONFIG_TEST_SET}-${CONFIG_TEST_SET}", want: "value-value"},
		{name: "unterminated", value: "${CONFIG_TEST_SET", wantErr: "unterminated"},
		{name: "empty-braces", value: "${}", wantErr: "empty"},
		{name: "malformed-scheme", value: "${:arg}", wantErr: "malformed"},
		{name: "fallback-on-a-scheme", value: "${CONFIG_TEST_SET:arg:-x}", wantErr: "fallback"},
		{name: "unset-without-fallback", value: "${CONFIG_TEST_ABSENT}", wantErr: "CONFIG_TEST_ABSENT"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expanded, err := ExpandDocument(context.Background(),
				[]byte("value: \""+test.value+"\"\n"))
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("ExpandDocument(%q) = nil error", test.value)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("err = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandDocument(%q): %v", test.value, err)
			}
			if !strings.Contains(string(expanded), `"`+test.want+`"`) {
				t.Errorf("expanded(%q) = %s, want %q", test.value, expanded, test.want)
			}
		})
	}
}

// An unregistered scheme is an error that lists what is registered, because the
// reader's question is "what may I write".
func TestExpandDocumentNamesTheOfferedSchemes(t *testing.T) {
	_, err := ExpandDocument(context.Background(), []byte("token: ${vault:key}\n"),
		WithResolver("secret", func(context.Context, []string) (string, error) { return "", nil }))
	if err == nil {
		t.Fatal("an unregistered scheme was accepted")
	}
	if !strings.Contains(err.Error(), "env, secret") {
		t.Errorf("err = %v, want the offered schemes listed", err)
	}
}

// A resolver's own failure is wrapped with the reference that produced it, so a
// five-scheme document says which lookup failed.
func TestExpandDocumentWrapsTheResolverError(t *testing.T) {
	_, err := ExpandDocument(context.Background(), []byte("token: ${secret:openai:luna}\n"),
		WithResolver("secret", func(context.Context, []string) (string, error) {
			return "", errTestResolver
		}))
	if err == nil {
		t.Fatal("a failing resolver was accepted")
	}
	if !strings.Contains(err.Error(), "secret:openai:luna") {
		t.Errorf("err = %v, want the reference named", err)
	}
}

// --- requirement ------------------------------------------------------

// A name written both bare and with a fallback is required: a fallback in one
// place does not excuse a missing value where there is none.
func TestRequiredPlaceholdersReportsARequiredNameOnce(t *testing.T) {
	requirement, err := RequiredPlaceholders(context.Background(), Layer{
		Priority: 0, Name: "a.yaml",
		Source: Bytes([]byte("name: ${TOKEN}\nother: ${TOKEN:-fallback}\n")),
	})
	if err != nil {
		t.Fatalf("RequiredPlaceholders: %v", err)
	}
	if len(requirement.Required) != 1 || requirement.Required[0] != "TOKEN" {
		t.Errorf("required = %v", requirement.Required)
	}
	for _, name := range requirement.Optional {
		if name == "TOKEN" {
			t.Errorf("TOKEN is optional and required at once: %v", requirement.Optional)
		}
	}
}

func TestRequiredPlaceholdersRejectsAMalformedLayer(t *testing.T) {
	if _, err := RequiredPlaceholders(context.Background(), Layer{
		Priority: 0, Name: "a.yaml", Source: Bytes([]byte("name: [unclosed\n")),
	}); err == nil {
		t.Fatal("a malformed layer was accepted")
	}
}

func TestRequiredPlaceholdersRejectsALayerWithTwoDocuments(t *testing.T) {
	if _, err := RequiredPlaceholders(context.Background(), Layer{
		Priority: 0, Name: "a.yaml", Source: Bytes([]byte("name: ${A}\n---\nname: ${B}\n")),
	}); err == nil {
		t.Fatal("a two-document layer was accepted")
	}
}

// A layer with no document has no references, and reporting that is not an
// error: the checklist is a description, not a gate.
func TestRequiredPlaceholdersToleratesAnEmptyLayer(t *testing.T) {
	requirement, err := RequiredPlaceholders(context.Background(), Layer{
		Priority: 0, Name: "a.yaml", Source: Bytes([]byte("# nothing\n")),
	})
	if err != nil {
		t.Fatalf("RequiredPlaceholders: %v", err)
	}
	if len(requirement.Required) != 0 || len(requirement.Optional) != 0 {
		t.Errorf("requirement = %+v, want nothing", requirement)
	}
}

func TestRequiredPlaceholdersDocumentToleratesAnEmptyDocument(t *testing.T) {
	requirement, err := RequiredPlaceholdersDocument([]byte("# nothing\n"), "deploy.yaml")
	if err != nil {
		t.Fatalf("RequiredPlaceholdersDocument: %v", err)
	}
	if len(requirement.Required) != 0 {
		t.Errorf("required = %v", requirement.Required)
	}
}

// --- provenance -------------------------------------------------------

// Each is a report loop, so stopping early must actually stop: a caller that
// prints one value must not pay for the whole map.
func TestProvenanceEachStopsWhenAsked(t *testing.T) {
	_, doc := mustLoad(t, base(), override("tools: {tty: true}\n"))
	prov := doc.Provenance()
	visited := 0
	prov.Each(func([]string, Ref) bool {
		visited++
		return false
	})
	if visited != 1 {
		t.Errorf("visited = %d, want the loop to stop after one value", visited)
	}
	// An unstored path is absent rather than zero-valued.
	if ref, ok := prov.Origin("no", "such", "path"); ok || ref != (Ref{}) {
		t.Errorf("Origin of a missing path = %+v, %v", ref, ok)
	}
}

// The zero Provenance is usable: a document built without layers still answers
// queries.
func TestProvenanceZeroValueAnswersQueries(t *testing.T) {
	var prov Provenance
	if _, ok := prov.Origin("name"); ok {
		t.Error("the zero Provenance reported a value")
	}
	if prov.Len() != 0 {
		t.Errorf("Len = %d", prov.Len())
	}
	prov.Each(func([]string, Ref) bool {
		t.Error("the zero Provenance visited a value")
		return false
	})
}

// errTestResolver is a resolver failure that is not an environment variable.
var errTestResolver = errors.New("vault is sealed")

// The requirement scan and the expansion share a parser, so both refuse a
// malformed document with the source named.
func TestExpandDocumentRejectsAMalformedDocument(t *testing.T) {
	if _, err := ExpandDocument(context.Background(), []byte("name: [unclosed\n")); err == nil {
		t.Fatal("a malformed document was accepted")
	}
}

func TestRequiredPlaceholdersDocumentRejectsAMalformedDocument(t *testing.T) {
	if _, err := RequiredPlaceholdersDocument([]byte("name: [unclosed\n"), "deploy.yaml"); err == nil {
		t.Fatal("a malformed document was accepted")
	}
}

// A document may be a sequence at the root: the requirement scan walks it, so a
// reference inside an element is still reported.
func TestRequiredPlaceholdersWalksASequenceDocument(t *testing.T) {
	requirement, err := RequiredPlaceholdersDocument([]byte("- ${FIRST}\n- plain\n- ${SECOND:-x}\n"), "list.yaml")
	if err != nil {
		t.Fatalf("RequiredPlaceholdersDocument: %v", err)
	}
	if got := strings.Join(requirement.Required, ","); got != "FIRST" {
		t.Errorf("required = %q, want FIRST", got)
	}
	if got := strings.Join(requirement.Optional, ","); got != "SECOND" {
		t.Errorf("optional = %q, want SECOND", got)
	}
	if got := requirement.Sources["FIRST"]; len(got) != 1 || got[0] != "list.yaml" {
		t.Errorf("sources = %v", got)
	}
}

// A source that fails is reported by the checklist too: the host is asking what
// it needs, and "I could not read the file" is an answer.
func TestRequiredPlaceholdersReportsAFailingSource(t *testing.T) {
	_, err := RequiredPlaceholders(context.Background(), Layer{
		Priority: 0, Name: "vault.yaml",
		Source: Func(func(context.Context) ([]byte, error) { return nil, errTestResolver }),
	})
	if err == nil {
		t.Fatal("a failing source was accepted")
	}
	if !strings.Contains(err.Error(), "vault.yaml") {
		t.Errorf("err = %v, want the layer named", err)
	}
}

// A load that said it does not want provenance keeps none: the report is empty
// rather than wrong, and the value still decodes.
func TestLoadWithoutProvenance(t *testing.T) {
	cfg, doc, err := Load[testConfig](context.Background(),
		[]Layer{base(), override("tools: {tty: true}\n")},
		Strict[testConfig], WithoutProvenance())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Tools.TTY {
		t.Error("the value did not decode")
	}
	if _, ok := doc.Origin("sandbox", "backend"); ok {
		t.Error("Origin answered with provenance disabled")
	}
	if doc.Provenance().Len() != 0 {
		t.Errorf("Provenance().Len() = %d, want 0", doc.Provenance().Len())
	}
	doc.Provenance().Each(func([]string, Ref) bool {
		t.Error("a disabled report visited a value")
		return false
	})
}

// A YAML 1.1 boolean is a boolean: the canonical fast path must not turn
// `enabled: yes` into the string "yes" and fail a document that used to load.
func TestLoadAcceptsYAML11Booleans(t *testing.T) {
	type document struct {
		Enabled bool `json:"enabled"`
	}
	for _, body := range []string{"enabled: yes\n", "enabled: on\n", "enabled: y\n", "enabled: TRUE\n"} {
		cfg, err := Decode[document](context.Background(), []byte(body), Strict[document])
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if !cfg.Enabled {
			t.Errorf("%q: enabled = false, want true", body)
		}
	}
	if _, err := Decode[document](context.Background(), []byte("enabled: off\n"), Strict[document]); err != nil {
		t.Fatalf("enabled: off: %v", err)
	}
}

// A merge key is expanded at parse time, so the three answers this package
// gives — Raw, Origin, Canonical — agree about a key it contributed.
func TestMergeKeyIsLiteralInTheTree(t *testing.T) {
	document, err := parseDocument([]byte(`
sandbox: &box
  backend: local
merged:
  <<: *box
  workdir: /w
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := expandMergeKeys(document); err != nil {
		t.Fatalf("expandMergeKeys: %v", err)
	}
	doc := &Doc{label: "merge", root: documentBody(document)}

	if value, ok := doc.Raw("merged", "backend"); !ok || value != "local" {
		t.Errorf("Raw(merged.backend) = %#v, %v", value, ok)
	}
	_ = doc
	canonical, err := doc.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if !strings.Contains(string(canonical), `"merged":{"backend":"local","workdir":"/w"}`) {
		t.Errorf("canonical = %s", canonical)
	}
}

// The rules the expansion implements: an explicit key wins, and an earlier
// source wins over a later one.
func TestMergeKeyPrecedence(t *testing.T) {
	cfg, _ := mustLoad(t,
		Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(
			"version: 1\nname: forge\nfiles: {roots: [/work]}\nsandbox: {backend: local}\n"))},
		Layer{Priority: 1, Name: "explicit.yaml", Source: Bytes([]byte(`
sandbox:
  <<: {backend: bwrap, workdir: /m}
  backend: local
`))},
	)
	if cfg.Sandbox.Backend != "local" {
		t.Errorf("explicit key did not win: backend = %q", cfg.Sandbox.Backend)
	}
	if cfg.Sandbox.WorkDir != "/m" {
		t.Errorf("merged key missing: workdir = %q", cfg.Sandbox.WorkDir)
	}

	cfg, _ = mustLoad(t, Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(`
version: 1
name: forge
files: {roots: [/work]}
sandbox:
  <<: [{backend: local}, {backend: bwrap}]
`))})
	if cfg.Sandbox.Backend != "local" {
		t.Errorf("earlier source did not win: backend = %q", cfg.Sandbox.Backend)
	}
}

// Provenance follows the layer that wrote the merge key, and never names the
// operator: the expanded tree is what the report describes.
func TestMergeKeyProvenanceNamesTheLayer(t *testing.T) {
	cfg, doc := mustLoad(t,
		Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(
			"version: 1\nname: forge\nfiles: {roots: [/work]}\nsandbox: {backend: local}\n"))},
		Layer{Priority: 10, Name: "merged.yaml", Source: Bytes([]byte(
			"sandbox:\n  <<: {workdir: /m}\n"))},
	)
	if cfg.Sandbox.WorkDir != "/m" {
		t.Fatalf("workdir = %q, want the merged value", cfg.Sandbox.WorkDir)
	}
	if ref, ok := doc.Origin("sandbox", "workdir"); !ok || ref.Name != "merged.yaml" {
		t.Errorf("Origin(sandbox.workdir) = %+v, %v", ref, ok)
	}
	if ref, ok := doc.Origin("sandbox", "backend"); !ok || ref.Name != "base.yaml" {
		t.Errorf("Origin(sandbox.backend) = %+v, %v", ref, ok)
	}
	doc.Provenance().Each(func(path []string, _ Ref) bool {
		for _, segment := range path {
			if segment == "<<" {
				t.Errorf("provenance recorded a merge key: %v", path)
			}
		}
		return true
	})
}

// A merge that merges nothing is refused rather than ignored: the author
// expected keys, and an empty document would hide that.
func TestMergeKeyRefusesANonMappingSource(t *testing.T) {
	_, _, err := load(t, Layer{Priority: 0, Name: "base.yaml", Source: Bytes([]byte(`
version: 1
name: forge
files: {roots: [/work]}
sandbox:
  <<: 7
`))})
	if err == nil {
		t.Fatal("a scalar merge source was accepted")
	}
	if !strings.Contains(err.Error(), "merge source") {
		t.Errorf("err = %v, want the merge source named", err)
	}
}

// The single-document path refuses a second document, so a host that prints a
// diagnostic never silently drops half of it.
func TestExpandDocumentRejectsMoreThanOneDocument(t *testing.T) {
	if _, err := ExpandDocument(context.Background(), []byte("a: 1\n---\nb: 2\n")); err == nil {
		t.Fatal("a two-document body was accepted")
	}
}

// WithProvenance puts back what WithoutProvenance took away, so an outer caller
// can override a set of options it did not write.
func TestWithProvenanceRestoresRecording(t *testing.T) {
	_, doc, err := Load[testConfig](context.Background(),
		[]Layer{base(), override("tools: {tty: true}\n")},
		Strict[testConfig], WithoutProvenance(), WithProvenance())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ref, ok := doc.Origin("sandbox", "backend"); !ok || ref.Name != "base.yaml" {
		t.Errorf("Origin = %+v, %v, want the base layer", ref, ok)
	}
}

// A merge source that aliases nothing is refused rather than dereferenced: the
// node is one only a hand-built tree can hold, which is why it is a unit test.
func TestMergeSourceWithoutAnAliasIsRefused(t *testing.T) {
	if _, err := mergeSourcePairs(&yaml.Node{Kind: yaml.AliasNode}); err == nil {
		t.Fatal("an alias without a target was accepted")
	}
}
