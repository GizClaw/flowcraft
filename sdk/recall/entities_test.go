package recall

import (
	"reflect"
	"testing"
)

// TestNormalizeEntities_AtomizesPhrasalEntities pins the headline
// behaviour: a phrasal entity coming from the LLM extractor
// produces (a) its normalised full-phrase form and (b) the per-token
// atoms that match the query side's capitalised-token / CJK
// extractor. Both must be present in the output so retrieval-time
// ContainsAny["entities"] fires regardless of which form the query
// pipeline emitted.
func TestNormalizeEntities_AtomizesPhrasalEntities(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "english phrasal entity",
			in:   []string{"Caroline's LGBTQ support group"},
			want: []string{"caroline", "caroline's lgbtq support group", "lgbtq"},
		},
		{
			name: "multi-word proper noun",
			in:   []string{"New York City"},
			want: []string{"city", "new", "new york city", "york"},
		},
		{
			name: "possessive normalisation",
			in:   []string{"Alice's birthday"},
			want: []string{"alice", "alice's birthday"},
		},
		{
			name: "cjk phrase atomises into runes + bigrams skipped (rune-level only here)",
			in:   []string{"黑咖啡 Alice"},
			want: []string{"alice", "黑咖啡", "黑咖啡 alice"},
		},
		{
			name: "drops leading question pronoun masquerading as proper noun",
			in:   []string{"What Day"},
			want: []string{"day", "what day"},
		},
		{
			name: "lowercase phrase preserved as full atom only",
			in:   []string{"matcha kit"},
			// no capitalised tokens, so only the full-phrase atom
			// survives; per-token splitting won't promote
			// lowercase content words.
			want: []string{"matcha kit"},
		},
		{
			name: "dedup across multiple sources",
			in:   []string{"Alice Martin", "Martin Alice", "alice martin"},
			want: []string{"alice", "alice martin", "martin", "martin alice"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeEntities(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("NormalizeEntities(%v)\n  got  %v\n  want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeEntities_EmptyAndStopwordOnly asserts that
// degenerate inputs produce nil / empty outputs rather than
// polluting the entity index with zero-information atoms.
func TestNormalizeEntities_EmptyAndStopwordOnly(t *testing.T) {
	for _, in := range [][]string{
		nil,
		{},
		{""},
		{"   "},
		// "what" / "the" are question pronouns / determiners that
		// only get harvested if they appear capitalised in a phrasal
		// entity; their bare form is filtered.
		{"what", "the"},
	} {
		if got := NormalizeEntities(in); len(got) != 0 {
			t.Errorf("expected empty result for %v, got %v", in, got)
		}
	}
}

// TestNormalizeEntities_BridgesIngestAndQuery is the integration-y
// assertion that the storage-side normalisation produces atoms the
// query-side `ruleEntities` extractor would emit for the same
// surface form. Without this contract the entity recall lane
// silently returns zero hits — the exact regression that motivated
// adding NormalizeEntities.
func TestNormalizeEntities_BridgesIngestAndQuery(t *testing.T) {
	// Stored entities as they arrive from the LLM extractor.
	stored := NormalizeEntities([]string{
		"Caroline's LGBTQ support group",
		"Tuesday",
	})
	storedSet := map[string]struct{}{}
	for _, e := range stored {
		storedSet[e] = struct{}{}
	}
	// Atoms a query-side extractor would emit for the matching
	// question (mirroring ruleEntities's output: lowercased,
	// capitalised-token-only).
	queryAtoms := []string{"caroline", "lgbtq", "tuesday"}
	for _, q := range queryAtoms {
		if _, ok := storedSet[q]; !ok {
			t.Errorf("query atom %q missing from stored entities %v", q, stored)
		}
	}
}
