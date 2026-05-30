package stopword_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/text/stopword"
)

// TestIsEnglish_BaselineHits exercises the canonical English
// stop-word table — the same 136-word baseline the legacy
// textsearch package shipped with, kept identical here to avoid
// drifting BM25 vocabularies on the consumer side.
func TestIsEnglish_BaselineHits(t *testing.T) {
	cases := []string{"the", "a", "an", "is", "are", "and", "or", "not", "of", "to"}
	for _, w := range cases {
		if !stopword.IsEnglish(w) {
			t.Errorf("%q should be in baseline stop-word set", w)
		}
	}
}

func TestIsEnglish_CaseInsensitive(t *testing.T) {
	for _, w := range []string{"THE", "Not", "AND", "to"} {
		if !stopword.IsEnglish(w) {
			t.Errorf("%q should match case-insensitively", w)
		}
	}
}

func TestIsEnglish_Misses(t *testing.T) {
	for _, w := range []string{"alice", "matcha", "go", "memory"} {
		if stopword.IsEnglish(w) {
			t.Errorf("%q should NOT be a stop word", w)
		}
	}
}

func TestIsCJKChar_BaselineHits(t *testing.T) {
	for _, r := range []rune{'的', '了', '在', '是', '我', '不'} {
		if !stopword.IsCJKChar(r) {
			t.Errorf("%q should be in baseline CJK stop-char set", string(r))
		}
	}
}

func TestIsCJKChar_Misses(t *testing.T) {
	for _, r := range []rune{'食', '猫', '海', 'a'} {
		if stopword.IsCJKChar(r) {
			t.Errorf("%q should NOT be a CJK stop char", string(r))
		}
	}
}

func TestMultilingualStopwords(t *testing.T) {
	for _, w := range []string{"cuándo", "où", "wann", "quando", "wanneer", "когда"} {
		if !stopword.IsMultilingual(w) {
			t.Errorf("%q should be in multilingual stop-word set", w)
		}
	}
	if stopword.IsMultilingual("alice") {
		t.Error("specific entity should not be multilingual stop-word")
	}
	if !stopword.MultilingualSet().Contains("the") || !stopword.MultilingualSet().Contains("когда") {
		t.Error("MultilingualSet should include English and multilingual entries")
	}
	if !stopword.CJKSet().Contains("的") || stopword.CJKSet().Contains("the") {
		t.Error("CJKSet should contain CJK stop characters, not English words")
	}
	if stopword.MultilingualOnlySet().Contains("the") || stopword.MultilingualOnlySet().Contains("will") {
		t.Error("MultilingualOnlySet should not include English baseline entries")
	}
	if !stopword.MultilingualOnlySet().Contains("的") || !stopword.MultilingualOnlySet().Contains("когда") {
		t.Error("MultilingualOnlySet should include CJK and non-English entries")
	}
}

// TestEnglishSet_IsCopy guarantees that successive EnglishSet
// calls return independent Sets — mutating one must not affect
// the package-level baseline or other consumers.
func TestEnglishSet_IsCopy(t *testing.T) {
	a := stopword.EnglishSet()
	b := stopword.EnglishSet()
	a.Add("zzz_domain_word")
	if b.Contains("zzz_domain_word") {
		t.Error("EnglishSet copies must be independent; mutation leaked")
	}
	if !stopword.IsEnglish("the") {
		t.Error("baseline IsEnglish must remain intact after copy mutation")
	}
}

// TestSet_AddExtendUnion covers the §2.3 Set API surface: chained
// Add / Extend / Union, all case-insensitive lookups via Contains.
func TestSet_AddExtendUnion(t *testing.T) {
	a := stopword.NewSet().Extend("Foo", "Bar")
	if !a.Contains("foo") || !a.Contains("BAR") {
		t.Errorf("Set.Contains should be case-insensitive, got a=%v", a)
	}
	b := stopword.NewSet().Add("baz")
	merged := a.Union(b)
	for _, w := range []string{"foo", "bar", "baz"} {
		if !merged.Contains(w) {
			t.Errorf("Union should contain %q", w)
		}
	}
	if a.Contains("baz") {
		t.Error("Union must not mutate the receiver")
	}
	if merged.Len() != 3 {
		t.Errorf("merged.Len() = %d, want 3", merged.Len())
	}
}

func TestSet_NilSafeContains(t *testing.T) {
	var s stopword.Set
	if s.Contains("anything") {
		t.Error("nil Set.Contains must return false")
	}
}
