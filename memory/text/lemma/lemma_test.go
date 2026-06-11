package lemma_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/text/lemma"
	snowball "github.com/GizClaw/flowcraft/memory/text/stem/adapter/snowball"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

// TestLemmatize_IrregularsCollapse asserts that the irregular-form
// table actually collapses high-frequency inflections that Porter
// alone leaves orphaned. Each row picks an inflection that a suffix
// stemmer mishandles (vowel-change pasts, suppletive forms, irregular
// noun plurals) and asserts that after Lemmatize + Stem the two
// surface forms share a BM25 key.
func TestLemmatize_IrregularsCollapse(t *testing.T) {
	cases := [][2]string{
		// be / have / do
		{"was", "be"}, {"were", "be"}, {"been", "be"}, {"is", "be"}, {"are", "be"},
		{"had", "have"}, {"has", "have"},
		{"did", "do"}, {"does", "do"}, {"done", "do"},
		// suppletive / vowel-change pasts
		{"went", "go"}, {"gone", "go"}, {"goes", "go"},
		{"came", "come"}, {"comes", "come"},
		{"ran", "run"},
		{"bought", "buy"}, {"buying", "buy"},
		{"brought", "bring"},
		{"caught", "catch"},
		{"taught", "teach"},
		{"thought", "think"}, {"thinks", "think"},
		{"sought", "seek"},
		{"fought", "fight"},
		{"said", "say"}, {"says", "say"},
		{"told", "tell"}, {"tells", "tell"},
		{"spoke", "speak"}, {"spoken", "speak"},
		{"heard", "hear"},
		{"saw", "see"}, {"seen", "see"},
		{"knew", "know"}, {"known", "know"},
		{"took", "take"}, {"taken", "take"},
		{"gave", "give"}, {"given", "give"},
		{"got", "get"}, {"gotten", "get"},
		{"made", "make"},
		{"sent", "send"},
		{"left", "leave"},
		{"lost", "lose"},
		{"found", "find"},
		{"chose", "choose"}, {"chosen", "choose"},
		{"ate", "eat"}, {"eaten", "eat"},
		{"drank", "drink"}, {"drunk", "drink"},
		{"slept", "sleep"},
		{"sat", "sit"},
		{"stood", "stand"},
		{"drove", "drive"}, {"driven", "drive"},
		{"flew", "fly"}, {"flown", "fly"},
		// irregular plurals
		{"children", "child"},
		{"men", "man"},
		{"women", "woman"},
		{"people", "person"},
		{"teeth", "tooth"},
		{"feet", "foot"},
		{"mice", "mouse"},
		{"geese", "goose"},
	}
	for _, c := range cases {
		a := snowball.Stem(lemma.Lemmatize(c[0]))
		b := snowball.Stem(lemma.Lemmatize(c[1]))
		if a != b {
			t.Errorf("snowball.Stem(lemma.Lemmatize(%q))=%q != snowball.Stem(lemma.Lemmatize(%q))=%q",
				c[0], a, c[1], b)
		}
	}
}

// TestLemmatize_NoOpForUnknownWords pins that proper nouns and
// out-of-vocabulary tokens pass through Lemmatize unchanged.
// (Regular conjugations of irregular base verbs — "comes", "running"
// — are intentionally kept in the table as fast-paths and so are
// excluded from this assertion. Porter would land on the same key,
// but the lookup is one map op cheaper than running the ruleset.)
func TestLemmatize_NoOpForUnknownWords(t *testing.T) {
	for _, w := range []string{
		"alice", "guangzhou", "matcha", "coffee", "tuesday",
		"discussed", "love", "loved",
	} {
		if got := lemma.Lemmatize(w); got != w {
			t.Errorf("lemma.Lemmatize(%q) = %q, want unchanged", w, got)
		}
	}
}

func TestLemmatizeLangScopesEnglishIrregulars(t *testing.T) {
	if got := lemma.LemmatizeLang("went", "english"); got != "go" {
		t.Fatalf("English LemmatizeLang = %q, want go", got)
	}
	if got := lemma.LemmatizeLang("went", "spanish"); got != "went" {
		t.Fatalf("Spanish LemmatizeLang should not apply English irregulars, got %q", got)
	}
	if got := lemma.LemmatizeEnglish("children"); got != "child" {
		t.Fatalf("LemmatizeEnglish = %q, want child", got)
	}
}

// TestLemmatize_TokenizerIntegration verifies that the SimpleTokenizer
// path emits the lemmatized + stemmed token, end to end, so callers
// querying BM25 with "what did Alice buy last week" find a fact
// stored as "Alice bought a matcha kit".
func TestLemmatize_TokenizerIntegration(t *testing.T) {
	tok := &tokenize.Simple{}
	q := tok.Tokenize("what did Alice buy last week")
	d := tok.Tokenize("Alice bought a matcha kit last week")
	qSet := map[string]struct{}{}
	for _, t := range q {
		qSet[t] = struct{}{}
	}
	overlapped := false
	for _, t := range d {
		if _, ok := qSet[t]; ok && (t == "bui" || t == "buy") {
			overlapped = true
			break
		}
	}
	if !overlapped {
		t.Fatalf("expected 'buy' / 'bought' to share a BM25 token; q=%v d=%v", q, d)
	}
}
