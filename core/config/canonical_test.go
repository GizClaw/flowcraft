package config

import (
	"strings"
	"testing"
)

// canonicalCorpus is what the two canonical paths must agree on. Both exist
// because the fast one cannot spell a non-string key, so the corpus is the
// contract between them: for every document here, either both succeed with the
// same bytes or both refuse. A YAML library upgrade that changed either path
// fails this test instead of silently changing a fingerprint.
var canonicalCorpus = []string{
	"version: 1\nname: forge\nfiles: {roots: [/work]}\n",
	"a: 1\nb: 1.0\nc: true\nd: null\ne: ~\n",
	"nested: {a: {b: {c: [1, 2, 3]}}}\n",
	"big: 18446744073709551616\nneg: -9223372036854775809\n",
	"tiny: 1e-320\nhugefloat: 1.7976931348623157e308\n",
	"anchor: &x {k: v}\nsecond: *x\n",
	"base: &b {k: v}\nmerged:\n  <<: *b\n  own: 1\n",
	"unicode: \"héllo → ünïcode\"\nquoted: \"a\\tb\"\n",
	"emptymap: {}\nemptyseq: []\nemptyobj: \"\"\n",
	"timestamp: 2001-12-15T02:59:43.1Z\n",
	"multi: |\n  line one\n  line two\n",
	"key with spaces: 1\n\"a.b\": 2\n",
	// The refusals: the slow path names the key, the fast path declines.
	"~: 1\ntwo: 2\n",
	"? [a]\n: 1\n",
	// Non-string keys that are spellable: only the slow path can succeed.
	"1: a\ntrue: b\n1.5: c\n",
	// YAML 1.1 booleans: yaml.v2 (the exact path) resolves these, yaml.v3 (the
	// fast path) keeps them strings. The fast path must decline, and the two
	// must still agree on the bytes the slow path renders.
	"enabled: yes\n", "enabled: OFF\n", "enabled: On\n", "enabled: n\n",
	"enabled: 'yes'\n", "n: 1\n", "y: 2\n",
	// Presentational styles: a layer written as JSON, one written as a flow
	// mapping, and one whose strings are quoted. Canonical bytes must not
	// depend on any of them.
	`{"version":1,"name":"json"}`,
	"{version: 1, name: forge}\n",
	"name: \"forge\"\nliteral: |\n  two\n  lines\n",
	// An empty collection is the one form whose block emission is still flow
	// ("{}"), so it is also the one that used to reach the JSON passthrough.
	"{}\n", "[]\n", "empty: {}\nseq: []\n",
	"floats: [1e3, .inf, -.inf, .nan, 3.14159]\n",
}

// TestCanonicalPathsAgree pins the fast path to the exact one. Two rules, one
// per direction: a document the fast path renders must render identically to the
// exact path, and a document the exact path refuses must never be accepted by
// the fast path. The other direction is allowed — a non-string key is spelled by
// the exact path alone, which is what the fallback in Canonical is for.
func TestCanonicalPathsAgree(t *testing.T) {
	for _, body := range canonicalCorpus {
		document, err := parseDocument([]byte(body))
		if err != nil {
			t.Fatalf("parse %q: %v", body, err)
		}
		doc := &Doc{label: "corpus", root: documentBody(document)}

		fast, fastOK := canonicalFromNode(doc.root)
		exact, exactErr := doc.canonicalViaYAML()

		if exactErr != nil && fastOK {
			t.Errorf("%q: the fast path accepted what the exact path refuses (%v)", body, exactErr)
			continue
		}
		if !fastOK {
			continue
		}
		if string(fast) != string(exact) {
			t.Errorf("%q:\n fast  = %s\n exact = %s", body, fast, exact)
		}
	}
}

// The exact path is what spells a non-string key: it is rare, and it is the
// reason the fast path declines instead of reporting a JSON encoding error.
func TestCanonicalSpellsANonStringKey(t *testing.T) {
	document, err := parseDocument([]byte("1: a\ntrue: b\n1.5: c\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc := &Doc{label: "corpus", root: documentBody(document)}
	canonical, err := doc.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	for _, want := range []string{`"1":"a"`, `"true":"b"`, `"1.5":"c"`} {
		if !strings.Contains(string(canonical), want) {
			t.Errorf("canonical = %s, want %s", canonical, want)
		}
	}
}

// A key that cannot be spelled is refused with the sentence that names it, not
// with a JSON encoder error: that sentence is what a reader can act on.
func TestCanonicalNamesAnUnspellableKey(t *testing.T) {
	document, err := parseDocument([]byte("version: 1\n~: 1\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc := &Doc{label: "corpus", root: documentBody(document)}
	_, err = doc.Canonical()
	if err == nil {
		t.Fatal("a null key was accepted")
	}
	if !strings.Contains(err.Error(), "cannot be spelled") || !strings.Contains(err.Error(), "corpus:") {
		t.Errorf("err = %v, want the key named and the document labelled", err)
	}
}

// The YAML 1.1 booleans are the reason the guard exists: the fast path keeps
// `yes` a string while the exact path makes it true, so the guard must decline
// for the plain spelling and may stay for the quoted one.
func TestCanonicalGuardYAML11Booleans(t *testing.T) {
	for body, wantFast := range map[string]bool{
		"enabled: yes\n":    false,
		"enabled: OFF\n":    false,
		"enabled: y\n":      false,
		"enabled: 'yes'\n":  true,
		"enabled: \"no\"\n": true,
		"enabled: true\n":   true,
		"name: forge\n":     true,
	} {
		document, err := parseDocument([]byte(body))
		if err != nil {
			t.Fatalf("parse %q: %v", body, err)
		}
		if _, ok := canonicalFromNode(documentBody(document)); ok != wantFast {
			t.Errorf("%q: fast path ok = %v, want %v", body, !wantFast, wantFast)
		}
	}
}

// Whatever the guard decides, Canonical must render what the exact path renders:
// that is the contract a fingerprint and a decode both rely on.
func TestCanonicalKeepsYAML11Meaning(t *testing.T) {
	document, err := parseDocument([]byte("enabled: yes\nother: 1\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc := &Doc{label: "yaml11", root: documentBody(document)}
	canonical, err := doc.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	exact, err := doc.canonicalViaYAML()
	if err != nil {
		t.Fatalf("canonicalViaYAML: %v", err)
	}
	if string(canonical) != string(exact) {
		t.Errorf("canonical = %s, exact = %s", canonical, exact)
	}
	if !strings.Contains(string(canonical), `"enabled":true`) {
		t.Errorf("canonical = %s, want the YAML 1.1 boolean", canonical)
	}
}
