package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/utils"
)

// TestJSONKeyOfMatchesLibrary pins the copied key rule against the library it
// was copied from. One key form per document keeps the conversion
// deterministic, so the expectation is exact rather than "one of two".
func TestJSONKeyOfMatchesLibrary(t *testing.T) {
	cases := []struct {
		document string
		want     string
	}{
		{"1: v\n", `{"1":"v"}`},            // int
		{"-7: v\n", `{"-7":"v"}`},          // negative int
		{"1.0: v\n", `{"1":"v"}`},          // float, formatted at float32
		{"1.5: v\n", `{"1.5":"v"}`},        // float that survives formatting
		{"1.0000000001: v\n", `{"1":"v"}`}, // collapsed to 1 by float32
		{"010: v\n", `{"8":"v"}`},          // YAML 1.1 octal
		{"true: v\n", `{"true":"v"}`},      // bool
		{"y: v\n", `{"true":"v"}`},         // YAML 1.1 boolean spelling
		{`"1": v` + "\n", `{"1":"v"}`},     // quoted string
		{".inf: v\n", `{".inf":"v"}`},      // float64 +Inf
		{"-.inf: v\n", `{"-.inf":"v"}`},    // float64 -Inf
		{".nan: v\n", `{".nan":"v"}`},      // float64 NaN
	}
	for _, tc := range cases {
		got, err := utils.ToJSON([]byte(tc.document))
		if err != nil {
			t.Errorf("%q: ToJSON: %v", tc.document, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%q: library says %s, this rule must say %s", tc.document, got, tc.want)
		}
	}
}

// TestJSONKeyOfUnsupportedForms records the key forms that are refused.
// jsonKeyOf reports them as "no key" so the collision walk stays out of the
// way: where the library errors, the result is stable and needs no flag.
//
// The two refusals come from different layers, and the wording is the boundary
// between core and the port: a nil key is one core now names itself ("YAML key
// !!null null cannot be spelled as a JSON key", #659) where the library it
// replaced printed the type with %s and produced "unsupported map key of type:
// %!s(<nil>)" (the port spells that "<nil>"), while a sequence key never gets
// that far -- yaml.v2 itself rejects it. Both are text-level differences for
// the port's error contract, so this test pins only the prefix, and accepts
// either wording for the key core names: it has to hold on both sides of #659,
// and what the port should say there is a decision of its own.
func TestJSONKeyOfUnsupportedForms(t *testing.T) {
	cases := []struct {
		document string
		want     []string
	}{
		{"null: v\n", []string{"unsupported map key of type:", "cannot be spelled as a JSON key"}},
		{"[a, b]: v\n", []string{"invalid map key"}},
	}
	for _, tc := range cases {
		_, err := utils.ToJSON([]byte(tc.document))
		if err == nil {
			t.Errorf("%q: expected a refusal, got none", tc.document)
			continue
		}
		matched := false
		for _, want := range tc.want {
			if strings.Contains(err.Error(), want) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("%q: error %v contains none of %q", tc.document, err, tc.want)
		}
	}
}

func TestKeyCollisions(t *testing.T) {
	cases := []struct {
		name     string
		document string
		want     []string
	}{
		{"int vs string", "1: a\n\"1\": b\n", []string{"1"}},
		{"int vs float", "1: a\n1.0: b\n", []string{"1"}},
		{"float32 collapse", "1: a\n1.0000000001: b\n", []string{"1"}},
		{"bool vs string", "true: a\n\"true\": b\n", []string{"true"}},
		{"octal vs decimal string", "010: a\n\"8\": b\n", []string{"8"}},
		{"inf vs string", ".inf: a\n\".inf\": b\n", []string{".inf"}},
		{"nested", "a:\n  1: one\n  \"1\": two\n", []string{"1"}},
		{"in a sequence", "- 1: a\n  \"1\": b\n", []string{"1"}},
		{"single non-string key", "1: only\n", nil},
		{"distinct keys", "1: a\n2: b\n", nil},
		{"json input is never collapsed", "{\"a\": 1}", nil},
		{"unparseable", "a: [\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keyCollisions([]byte(tc.document)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("keyCollisions(%q) = %v, want %v", tc.document, got, tc.want)
			}
		})
	}
}

// TestToJSONRefusesCollapsedKeys pins the conversion side of the evidence
// TestKeyCollisions collects: where keyCollisions names the collapse, ToJSON
// must refuse the document, and the refusal must name the same JSON key. The
// spellings match the cases above, so the two tests describe one class from
// both ends.
func TestToJSONRefusesCollapsedKeys(t *testing.T) {
	cases := []struct {
		name     string
		document string
		wantKey  string
	}{
		{"int vs string", "1: a\n\"1\": b\n", "1"},
		{"bool vs string", "true: a\n\"true\": b\n", "true"},
		{"int vs float", "1: a\n1.0: b\n", "1"},
		{"float32 collapse", "1: a\n1.0000000001: b\n", "1"},
		{"octal vs decimal string", "010: a\n\"8\": b\n", "8"},
		{"inf vs string", ".inf: a\n\".inf\": b\n", ".inf"},
		{"nested", "a:\n  1: one\n  \"1\": two\n", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := utils.ToJSON([]byte(tc.document))
			if err == nil {
				t.Fatalf("ToJSON(%q) accepted a collapsed mapping", tc.document)
			}
			want := "convert to the same JSON key \"" + tc.wantKey + "\""
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ToJSON(%q) error %v does not contain %q", tc.document, err, want)
			}
		})
	}
}

// TestToJSONKeepsNonCollapsingKeys is the control beside the refusals above: a
// document whose non-string keys spell distinct JSON keys still converts, so
// the refusal is about the collapse and not about non-string keys as such
// (fixture 51's standalone case, with two keys to order).
func TestToJSONKeepsNonCollapsingKeys(t *testing.T) {
	document := "1: one\n2: two\ntrue: tk\n"
	got, err := utils.ToJSON([]byte(document))
	if err != nil {
		t.Fatalf("ToJSON(%q) = %v, want a conversion", document, err)
	}
	want := `{"1":"one","2":"two","true":"tk"}`
	if string(got) != want {
		t.Errorf("ToJSON(%q) = %s, want %s", document, got, want)
	}
}
