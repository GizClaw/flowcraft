package utils

import (
	"encoding/json"
	"strings"
	"testing"

	yamlv2 "gopkg.in/yaml.v2"
	"sigs.k8s.io/yaml"
)

type testDoc struct {
	Version   string `json:"version"`
	MaxRevise int    `json:"max_revise"`
}

func TestFormatOf(t *testing.T) {
	tests := []struct {
		name string
		data string
		want Format
	}{
		{name: "json object", data: `{"version":"v1"}`, want: FormatJSON},
		{name: "json object with leading whitespace", data: " \t\n{\"a\":1}", want: FormatJSON},
		{name: "yaml mapping", data: "version: v1\n", want: FormatYAML},
		{name: "yaml list", data: "- a\n- b\n", want: FormatYAML},
		{name: "json array is yaml by k8s rule", data: `[1,2]`, want: FormatYAML},
		{name: "empty document", data: "", want: FormatYAML},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatOf([]byte(tt.data)); got != tt.want {
				t.Fatalf("FormatOf(%q) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

func TestToJSON_YAML(t *testing.T) {
	data, err := ToJSON([]byte("version: v1\nmax_revise: 2\n"))
	if err != nil {
		t.Fatalf("ToJSON error: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("converted output is not JSON: %v", err)
	}
	if decoded["version"] != "v1" || decoded["max_revise"] != float64(2) {
		t.Fatalf("converted = %v", decoded)
	}
}

func TestToJSON_JSONPassthrough(t *testing.T) {
	input := []byte(`{"version":"v1"}`)
	data, err := ToJSON(input)
	if err != nil {
		t.Fatalf("ToJSON error: %v", err)
	}
	if string(data) != string(input) {
		t.Fatalf("ToJSON = %q, want passthrough %q", data, input)
	}
}

func TestToJSON_RejectsInvalidJSON(t *testing.T) {
	if _, err := ToJSON([]byte(`{"version":`)); err == nil {
		t.Fatal("ToJSON accepted an invalid JSON document")
	}
}

func TestDecode_YAML(t *testing.T) {
	doc, err := Decode[testDoc]([]byte("version: v1\nmax_revise: 2\n"))
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if doc.Version != "v1" || doc.MaxRevise != 2 {
		t.Fatalf("Decode = %+v", doc)
	}
}

func TestDecode_JSON(t *testing.T) {
	doc, err := Decode[testDoc]([]byte(`{"version":"v1","max_revise":2}`))
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if doc.Version != "v1" || doc.MaxRevise != 2 {
		t.Fatalf("Decode = %+v", doc)
	}
}

func TestDecode_RejectsUnknownFields(t *testing.T) {
	for _, data := range []string{
		`{"version":"v1","unknown":1}`,
		"version: v1\nunknown: 1\n",
	} {
		if _, err := Decode[testDoc]([]byte(data)); err == nil {
			t.Fatalf("Decode accepted unknown field in %q", data)
		} else if !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("Decode error = %v, want unknown-field error", err)
		}
	}
}

func TestDecode_RejectsTrailingDocument(t *testing.T) {
	if _, err := Decode[testDoc]([]byte(`{"version":"v1"} {"version":"v2"}`)); err == nil {
		t.Fatal("Decode accepted multiple JSON documents")
	}
	if _, err := Decode[testDoc]([]byte("version: v1\n---\nversion: v2\n")); err == nil {
		t.Fatal("Decode accepted multiple YAML documents")
	}
}

func TestJSONKeySpelling_MatchesLibrary(t *testing.T) {
	// The refusal finds a collapse by spelling each key itself
	// (jsonKeySpelling) and comparing the result with the library's own
	// spelling of the same document, so a copy that drifted would stop
	// finding collapses while every test below still passed. Hence this test
	// reads the library's answer back out of its conversion, form by form:
	// the float64 rows are where drift is easiest, since float32 precision,
	// the exponent form and the edges of the float32 range all live there.
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "string", data: "\"1\": a\n", want: "1"},
		{name: "int", data: "1: a\n", want: "1"},
		{name: "int64 range", data: "9223372036854775807: a\n", want: "9223372036854775807"},
		{name: "float", data: "1.5: a\n", want: "1.5"},
		{name: "integral float", data: "1.0: a\n", want: "1"},
		{name: "float past float32 precision", data: "1.0000000001: a\n", want: "1"},
		{name: "float rounded up by float32", data: "16777217.0: a\n", want: "1.6777216e+07"},
		{name: "float in exponent form", data: "1e6: a\n", want: "1e+06"},
		{name: "small float in exponent form", data: "0.000001: a\n", want: "1e-06"},
		{name: "float at the float32 subnormal edge", data: "1e-45: a\n", want: "1e-45"},
		{name: "float above the float32 range", data: "1e40: a\n", want: ".inf"},
		{name: "float below the float32 range", data: "1e-50: a\n", want: "0"},
		{name: "yaml 1.1 octal", data: "010: a\n", want: "8"},
		{name: "bool", data: "true: a\n", want: "true"},
		{name: "false", data: "false: a\n", want: "false"},
		{name: "infinity", data: ".inf: a\n", want: ".inf"},
		{name: "negative infinity", data: "-.inf: a\n", want: "-.inf"},
		{name: "not a number", data: ".nan: a\n", want: ".nan"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The library's spelling, straight out of the library.
			converted, err := ToJSON([]byte(tt.data))
			if err != nil {
				t.Fatalf("ToJSON(%q) error: %v", tt.data, err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(converted, &decoded); err != nil {
				t.Fatalf("converted %q to %s, which is not a JSON object: %v", tt.data, converted, err)
			}
			if len(decoded) != 1 {
				t.Fatalf("ToJSON(%q) = %s, want a single key", tt.data, converted)
			}
			var libraryKey string
			for key := range decoded {
				libraryKey = key
			}
			if libraryKey != tt.want {
				t.Fatalf("ToJSON(%q) spelled the key %q, want %q", tt.data, libraryKey, tt.want)
			}

			// The key the refusal sees, out of the same yaml.v2 decode the
			// single-document check performs.
			var document any
			if err := yamlv2.NewDecoder(strings.NewReader(tt.data)).Decode(&document); err != nil {
				t.Fatalf("decoding %q for the refusal: %v", tt.data, err)
			}
			mapping, ok := document.(map[interface{}]interface{})
			if !ok {
				t.Fatalf("decoded %q to %T, want a mapping", tt.data, document)
			}
			if len(mapping) != 1 {
				t.Fatalf("decoded %q to %d keys, want 1", tt.data, len(mapping))
			}
			for key := range mapping {
				spelled, ok := jsonKeySpelling(key)
				if !ok {
					t.Fatalf("jsonKeySpelling(%#v) refused the key of %q", key, tt.data)
				}
				if spelled != libraryKey {
					t.Fatalf("jsonKeySpelling(%#v of %T) = %q, but the library spells %q as %q",
						key, key, spelled, tt.data, libraryKey)
				}
			}
		})
	}
}

func TestToJSON_RejectsCollapsingKeys(t *testing.T) {
	// Every document below holds two distinct YAML keys that the conversion
	// to JSON spells the same way, so which of the two values survives is
	// decided by map iteration order: the document has no defined conversion
	// and is refused rather than converted.
	tests := []struct {
		name    string
		data    string
		jsonKey string
	}{
		{name: "int and its string", data: "1: a\n\"1\": b\n", jsonKey: `"1"`},
		{name: "bool and its string", data: "true: a\n\"true\": b\n", jsonKey: `"true"`},
		{name: "false and its string", data: "false: a\n\"false\": b\n", jsonKey: `"false"`},
		{name: "int and an equal float", data: "1: a\n1.0: b\n", jsonKey: `"1"`},
		{name: "int and a float past float32 precision", data: "1: a\n1.0000000001: b\n", jsonKey: `"1"`},
		{name: "float above the float32 range and the infinity string", data: "1e40: a\n\".inf\": b\n", jsonKey: `".inf"`},
		{name: "float below the float32 range and an equal int", data: "1e-50: a\n0: b\n", jsonKey: `"0"`},
		{name: "float below the float32 range and its string", data: "1e-50: a\n\"0\": b\n", jsonKey: `"0"`},
		{name: "float in exponent form and its string", data: "1e6: a\n\"1e+06\": b\n", jsonKey: `"1e+06"`},
		{name: "small float in exponent form and its string", data: "0.000001: a\n\"1e-06\": b\n", jsonKey: `"1e-06"`},
		{name: "float rounded up by float32 and its string", data: "16777217.0: a\n\"1.6777216e+07\": b\n", jsonKey: `"1.6777216e+07"`},
		{name: "yaml 1.1 octal and its decimal string", data: "010: a\n\"8\": b\n", jsonKey: `"8"`},
		{name: "int64 range and its string", data: "9223372036854775807: a\n\"9223372036854775807\": b\n", jsonKey: `"9223372036854775807"`},
		{name: "infinity and its string", data: ".inf: a\n\".inf\": b\n", jsonKey: `".inf"`},
		{name: "negative infinity and its string", data: "-.inf: a\n\"-.inf\": b\n", jsonKey: `"-.inf"`},
		{name: "not a number and its string", data: ".nan: a\n\".nan\": b\n", jsonKey: `".nan"`},
		{name: "nested mapping", data: "outer:\n  1: a\n  \"1\": b\n", jsonKey: `"1"`},
		{name: "mapping in a sequence", data: "list:\n  - 1: a\n    \"1\": b\n", jsonKey: `"1"`},
		{name: "collapsing pair with equal values", data: "1: a\n\"1\": a\n", jsonKey: `"1"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const want = "config utils: convert YAML to JSON: YAML keys "
			_, err := ToJSON([]byte(tt.data))
			if err == nil {
				t.Fatalf("ToJSON(%q) accepted a document with no defined conversion", tt.data)
			}
			if !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("ToJSON(%q) error = %v, want a refusal starting with %q", tt.data, err, want)
			}
			if !strings.Contains(err.Error(), tt.jsonKey) {
				t.Fatalf("ToJSON(%q) error = %v, want it to name the JSON key %s", tt.data, err, tt.jsonKey)
			}
		})
	}
}

func TestToJSON_CollisionRefusalIsStable(t *testing.T) {
	// The refusal has to be a function of the document as much as the
	// conversion is: the collision is found by iterating a Go map, so an
	// unsorted message would leak that order into the error string and make
	// the refusal itself flaky. Repeated because a single run of a map
	// iteration proves nothing.
	const data = "1: a\n\"1\": b\ntrue: c\n\"true\": d\n"
	const want = `config utils: convert YAML to JSON: ` +
		`YAML keys !!int 1, !!str "1" convert to the same JSON key "1"; ` +
		`YAML keys !!bool true, !!str "true" convert to the same JSON key "true"`
	for i := 0; i < 64; i++ {
		_, err := ToJSON([]byte(data))
		if err == nil {
			t.Fatalf("ToJSON(%q) accepted a document with two collisions", data)
		}
		if err.Error() != want {
			t.Fatalf("ToJSON(%q) error = %v, want %v", data, err, want)
		}
	}
}

func TestToJSON_ReportsACollisionOnce(t *testing.T) {
	// The colliding mapping is written once and aliased twice, and yaml.v2
	// decodes an alias into a copy, so the walk finds three equal mappings
	// holding one collision. The refusal names it once: the sentence is about
	// the keys, not about the places they were found in.
	const data = "base: &b\n  1: x\n  \"1\": y\ncopy: *b\nagain: *b\n"
	const want = `config utils: convert YAML to JSON: ` +
		`YAML keys !!int 1, !!str "1" convert to the same JSON key "1"`
	_, err := ToJSON([]byte(data))
	if err == nil {
		t.Fatalf("ToJSON(%q) accepted a document with no defined conversion", data)
	}
	if err.Error() != want {
		t.Fatalf("ToJSON(%q) error = %v, want %v", data, err, want)
	}
}

func TestToJSON_CollapsingKeysAreRefusedNotResolved(t *testing.T) {
	const data = "1: a\n\"1\": b\n"
	// The library resolves this document silently, and to two different
	// documents: 353 runs of one form against 47 of the other, measured over
	// 400, which is the start-offset distribution of a small Go map. The
	// refusal above exists because that is not a decision anybody made.
	resolved := map[string]bool{}
	for i := 0; i < 200 && len(resolved) < 2; i++ {
		var value any
		if err := yaml.UnmarshalStrict([]byte(data), &value); err != nil {
			t.Fatalf("yaml.UnmarshalStrict(%q) error: %v", data, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("json.Marshal(%q) error: %v", data, err)
		}
		resolved[string(encoded)] = true
	}
	if len(resolved) != 2 {
		t.Fatalf("the library resolved %q to a single form in 200 runs (%v); "+
			"if it can no longer collapse keys, the refusal is dead code", data, resolved)
	}
	if _, err := ToJSON([]byte(data)); err == nil {
		t.Fatalf("ToJSON(%q) resolved the collapse instead of refusing it", data)
	}
}

func TestToJSON_AcceptsKeysThatStayDistinct(t *testing.T) {
	tests := []struct{ name, data string }{
		{name: "int and a different string", data: "1: a\n\"2\": b\n"},
		{name: "float and a different string", data: "1.5: a\n\"2.5\": b\n"},
		{name: "one key per mapping of a sequence", data: "list:\n  - 1: a\n  - \"1\": b\n"},
		{name: "string keys differing in case", data: "Name: a\nname: b\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ToJSON([]byte(tt.data)); err != nil {
				t.Fatalf("ToJSON(%q) error: %v", tt.data, err)
			}
		})
	}
	// The JSON path is returned verbatim, duplicate keys included: encoding/json
	// gives those to the last one, which is a rule with a defined answer, not
	// map iteration order. Only the YAML path is refused.
	const duplicate = `{"1":"a","1":"b"}`
	data, err := ToJSON([]byte(duplicate))
	if err != nil {
		t.Fatalf("ToJSON(%q) error: %v", duplicate, err)
	}
	if string(data) != duplicate {
		t.Fatalf("ToJSON(%q) = %s, want the document back", duplicate, data)
	}
}

func TestToJSON_KeepsRefusingKeysTheLibraryRefuses(t *testing.T) {
	// A document yaml.v2 itself cannot decode, or that fails the strict
	// decode, keeps that error: the key findings only name a document that
	// reached the conversion and could not be converted. A colliding pair
	// sitting next to the failure does not preempt it either.
	tests := []struct{ name, data, want string }{
		{name: "sequence key", data: "? [a]\n: b\n", want: "invalid map key"},
		{name: "mapping key", data: "? {a: b}\n: c\n", want: "invalid map key"},
		{name: "duplicate key and a collision", data: "1: a\n1: b\n\"2\": c\n2: d\n", want: "already set in map"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ToJSON([]byte(tt.data))
			if err == nil {
				t.Fatalf("ToJSON(%q) accepted a document the library refuses", tt.data)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ToJSON(%q) error = %v, want the library's %q", tt.data, err, tt.want)
			}
			for _, ours := range []string{"convert to the same JSON key", "cannot be spelled"} {
				if strings.Contains(err.Error(), ours) {
					t.Fatalf("ToJSON(%q) error = %v, want the library's error to win", tt.data, err)
				}
			}
		})
	}
}

func TestDecode_RejectsCollapsingKeys(t *testing.T) {
	// Decode is the entry point the configuration loaders use, and the
	// refusal has to reach it before the collapsed document can be decoded
	// into a typed struct.
	_, err := Decode[testDoc]([]byte("1: a\n\"1\": b\n"))
	if err == nil {
		t.Fatal("Decode accepted a document with no defined conversion")
	}
	if !strings.Contains(err.Error(), "convert to the same JSON key") {
		t.Fatalf("Decode error = %v, want the collision refusal", err)
	}
}

func TestToJSON_UnusableKeyRefusalIsStable(t *testing.T) {
	// A document holding two keys the conversion cannot spell also makes the
	// library's own answer unstable: it reports the first one its map walk
	// meets, 362 runs naming `~` and 38 the integer over 400. Both are named
	// instead, in the sorted order of their spelling, and the sentence is the
	// same one every run.
	const data = "~: A\n18446744073709551615: B\n"
	const want = "config utils: convert YAML to JSON: " +
		"YAML keys !!int 18446744073709551615, !!null null cannot be spelled as JSON keys"
	for i := 0; i < 128; i++ {
		_, err := ToJSON([]byte(data))
		if err == nil {
			t.Fatalf("ToJSON(%q) accepted a document with two unspellable keys", data)
		}
		if err.Error() != want {
			t.Fatalf("ToJSON(%q) error = %v, want %v", data, err, want)
		}
	}
}

func TestToJSON_NamesEveryUnusableKey(t *testing.T) {
	// Unspellable keys are found wherever they sit — under a sequence, in a
	// nested mapping — and the report names every distinct one, sorted, so it
	// does not depend on where the walk looked first. A collision next to one
	// does not win the report: the unspellable key is the condition no
	// conversion of this document could ever meet.
	const nested = "outer:\n  - 18446744073709551615: A\nnull: B\n"
	const want = "config utils: convert YAML to JSON: " +
		"YAML keys !!int 18446744073709551615, !!null null cannot be spelled as JSON keys"
	_, err := ToJSON([]byte(nested))
	if err == nil {
		t.Fatalf("ToJSON(%q) accepted a document with nested unspellable keys", nested)
	}
	if err.Error() != want {
		t.Fatalf("ToJSON(%q) error = %v, want %v", nested, err, want)
	}
	const mixed = "18446744073709551615: a\n\"18446744073709551615\": b\n"
	const wantOne = "config utils: convert YAML to JSON: " +
		"YAML key !!int 18446744073709551615 cannot be spelled as a JSON key"
	_, err = ToJSON([]byte(mixed))
	if err == nil {
		t.Fatalf("ToJSON(%q) accepted a document with an unspellable key", mixed)
	}
	if err.Error() != wantOne {
		t.Fatalf("ToJSON(%q) error = %v, want %v", mixed, err, wantOne)
	}
	// A bare null key, on its own: the singular sentence, naming it.
	const bare = "~: A\n"
	const wantBare = "config utils: convert YAML to JSON: " +
		"YAML key !!null null cannot be spelled as a JSON key"
	_, err = ToJSON([]byte(bare))
	if err == nil {
		t.Fatalf("ToJSON(%q) accepted a document with a null key", bare)
	}
	if err.Error() != wantBare {
		t.Fatalf("ToJSON(%q) error = %v, want %v", bare, err, wantBare)
	}
	// `~` and `null` are one key to yaml.v2 and one spelling here, so the two
	// ways of writing the same unspellable key read as one key, not two.
	const spelledTwice = "~: A\nnull: B\n"
	_, err = ToJSON([]byte(spelledTwice))
	if err == nil {
		t.Fatalf("ToJSON(%q) accepted a document with a null key", spelledTwice)
	}
	if err.Error() != wantBare {
		t.Fatalf("ToJSON(%q) error = %v, want %v", spelledTwice, err, wantBare)
	}
	// An unspellable key arriving through a merge is in the decoded mapping
	// like any other, because that is the shape the conversion reads.
	const merged = "base: &base\n  ~: A\nderived:\n  <<: *base\n"
	_, err = ToJSON([]byte(merged))
	if err == nil {
		t.Fatalf("ToJSON(%q) accepted a merge carrying a null key", merged)
	}
	if err.Error() != wantBare {
		t.Fatalf("ToJSON(%q) error = %v, want %v", merged, err, wantBare)
	}
}

func TestToJSON_LibraryDisagreesWithItselfOnUnusableKeys(t *testing.T) {
	// Measures the noise being replaced, the way the collision sibling does
	// for the coin flip it refuses: the library's message for this document
	// is decided by map iteration order, so no caller can act on its wording.
	const data = "~: A\n18446744073709551615: B\n"
	seen := map[string]bool{}
	for i := 0; i < 400 && len(seen) < 2; i++ {
		var value any
		err := yaml.UnmarshalStrict([]byte(data), &value)
		if err == nil {
			t.Fatalf("yaml.UnmarshalStrict(%q) converted a document it cannot spell", data)
		}
		seen[err.Error()] = true
	}
	if len(seen) != 2 {
		t.Fatalf("the library refused %q in one wording across 400 runs (%v); "+
			"if it no longer walks a map here, the refusal is dead code", data, seen)
	}
}

func TestUnusableKeys_MatchesTheLibrary(t *testing.T) {
	// The set of keys the refusal names is the set the conversion refuses: a
	// document the library converts is never refused, a key it refuses for
	// being unspellable is named by us, and a key yaml.v2 cannot hash keeps
	// its own `invalid map key` error because the decode fails there first.
	tests := []struct {
		name string
		data string
		want string // "converts", "unspellable" or "unhashable"
	}{
		{name: "int key", data: "1: a\n", want: "converts"},
		{name: "string key", data: "\"1\": a\n", want: "converts"},
		{name: "bool key", data: "true: a\n", want: "converts"},
		{name: "float key", data: "1.5: a\n", want: "converts"},
		{name: "yaml 1.1 octal key", data: "010: a\n", want: "converts"},
		{name: "infinity key", data: ".inf: a\n", want: "converts"},
		{name: "int64 max key", data: "9223372036854775807: a\n", want: "converts"},
		// Past uint64 the value is no longer an integer to yaml.v2: it
		// resolves to a float, and a float is a key the conversion spells.
		// The unspellable class ends at uint64, so this row is the boundary.
		{name: "integer past uint64", data: "18446744073709551616: a\n", want: "converts"},
		{name: "null key", data: "~: a\n", want: "unspellable"},
		{name: "null spelled out", data: "null: a\n", want: "unspellable"},
		{name: "integer past int64", data: "18446744073709551615: a\n", want: "unspellable"},
		{name: "sequence key", data: "? [a]\n: b\n", want: "unhashable"},
		{name: "mapping key", data: "? {a: b}\n: c\n", want: "unhashable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var value any
			libraryErr := yaml.UnmarshalStrict([]byte(tt.data), &value)
			_, err := ToJSON([]byte(tt.data))
			switch tt.want {
			case "converts":
				if libraryErr != nil {
					t.Fatalf("the library refused %q: %v; the row is stale", tt.data, libraryErr)
				}
				if err != nil {
					t.Fatalf("ToJSON(%q) error: %v, want it converted", tt.data, err)
				}
			case "unspellable":
				if libraryErr == nil || !strings.Contains(libraryErr.Error(), "unsupported map key") {
					t.Fatalf("the library on %q = %v, want an unsupported-key refusal", tt.data, libraryErr)
				}
				if err == nil || !strings.Contains(err.Error(), "cannot be spelled as") {
					t.Fatalf("ToJSON(%q) error = %v, want the key named by us", tt.data, err)
				}
			case "unhashable":
				if libraryErr == nil || !strings.Contains(libraryErr.Error(), "invalid map key") {
					t.Fatalf("the library on %q = %v, want an invalid-map-key refusal", tt.data, libraryErr)
				}
				if err == nil || !strings.Contains(err.Error(), "invalid map key") {
					t.Fatalf("ToJSON(%q) error = %v, want the library's refusal", tt.data, err)
				}
			}
		})
	}
}
