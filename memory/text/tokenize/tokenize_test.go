package tokenize_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/text/stopword"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

func TestSimple(t *testing.T) {
	tok := &tokenize.Simple{}
	tokens := tok.Tokenize("Hello World programming test")
	if len(tokens) == 0 {
		t.Fatal("expected tokens")
	}
	for _, tk := range tokens {
		if stopword.IsEnglish(tk) {
			t.Fatalf("stop word %q should have been filtered", tk)
		}
	}
}

func TestSimple_StopWords(t *testing.T) {
	tok := &tokenize.Simple{}
	tokens := tok.Tokenize("the a an is are to in for")
	if len(tokens) != 0 {
		t.Fatalf("expected 0 tokens (all stop words), got %d: %v", len(tokens), tokens)
	}
}

func TestSimple_ShortTokens(t *testing.T) {
	tok := &tokenize.Simple{}
	tokens := tok.Tokenize("a b c d go")
	for _, tk := range tokens {
		if len(tk) < 2 {
			t.Fatalf("token %q is too short", tk)
		}
	}
}

func TestCJKBigram_Chinese(t *testing.T) {
	tok := &tokenize.CJKBigram{}
	tokens := tok.Tokenize("你好世界 hello")
	if len(tokens) == 0 {
		t.Fatal("expected tokens")
	}
	hasBigram := false
	for _, tk := range tokens {
		if len([]rune(tk)) == 2 {
			hasBigram = true
			break
		}
	}
	if !hasBigram {
		t.Fatal("expected CJK bigrams")
	}
}

func TestDetect_ASCII(t *testing.T) {
	tok := tokenize.Detect("hello world test")
	if _, ok := tok.(*tokenize.Simple); !ok {
		t.Fatal("expected *tokenize.Simple for ASCII text")
	}
}

func TestDetect_CJK(t *testing.T) {
	tok := tokenize.Detect("这是中文内容")
	if _, ok := tok.(*tokenize.CJKBigram); !ok {
		t.Fatal("expected *tokenize.CJKBigram for CJK text")
	}
}

func TestIsCJK(t *testing.T) {
	if !tokenize.IsCJK('你') {
		t.Fatal("expected '你' to be CJK")
	}
	if tokenize.IsCJK('a') {
		t.Fatal("expected 'a' to not be CJK")
	}
}

func TestCJKBigram_StopChars(t *testing.T) {
	tok := &tokenize.CJKBigram{}
	tokens := tok.Tokenize("的了在是")
	if len(tokens) != 0 {
		t.Fatalf("expected 0 tokens (all CJK stop chars), got %d: %v", len(tokens), tokens)
	}
}

func TestCJKBigram_MixedWithStopChars(t *testing.T) {
	tok := &tokenize.CJKBigram{}
	tokens := tok.Tokenize("知识的检索")
	for _, tk := range tokens {
		for _, r := range tk {
			if stopword.IsCJKChar(r) {
				t.Fatalf("token %q contains CJK stop char", tk)
			}
		}
	}
	if len(tokens) == 0 {
		t.Fatal("expected non-empty tokens after filtering CJK stop chars")
	}
}

func TestSimple_ExpandedStopWords(t *testing.T) {
	tok := &tokenize.Simple{}
	expanded := []string{"but", "if", "so", "about", "because", "while", "during", "between", "also", "very"}
	for _, sw := range expanded {
		tokens := tok.Tokenize(sw)
		if len(tokens) != 0 {
			t.Fatalf("expected %q to be filtered as stop word, got %v", sw, tokens)
		}
	}
}

func TestSimpleMultilingualStopwordsAndStemming(t *testing.T) {
	tok := tokenize.NewMultilingual()
	tokens := tok.Tokenize("les opciones running кошки went")
	if contains(tokens, "les") {
		t.Fatalf("expected French stopword to be filtered, got %v", tokens)
	}
	for _, want := range []string{"opcion", "run", "кошк", "go"} {
		if !contains(tokens, want) {
			t.Fatalf("expected %q in multilingual tokens, got %v", want, tokens)
		}
	}
}

func TestSimpleLanguageSpecificLemmaDoesNotLeakEnglish(t *testing.T) {
	tok := &tokenize.Simple{
		Stopwords:     stopword.NewSet(),
		StemLanguages: []string{"spanish"},
	}
	tokens := tok.Tokenize("went")
	if !contains(tokens, "went") || contains(tokens, "go") {
		t.Fatalf("Spanish-only tokenization should not apply English irregular lemma, got %v", tokens)
	}
}

// TestSplitWords covers the §2.3 helper: raw word boundaries with
// case preserved, no stop-word filtering, no morphology folding.
// This is the primitive callers reach for when they need NER-grade
// surface forms instead of BM25 vocabulary keys.
func TestSplitWords(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "Alice", []string{"Alice"}},
		{"preserves_case", "Alice met Bob in Paris", []string{"Alice", "met", "Bob", "in", "Paris"}},
		{"keeps_short_tokens", "I am 32", []string{"I", "am", "32"}},
		{"splits_on_punct", "hello, world! how-are-you?", []string{"hello", "world", "how", "are", "you"}},
		{"cjk_runs_split_by_punct", "你好,世界", []string{"你好", "世界"}},
		{"only_punct", "!@#$", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenize.SplitWords(tc.in)
			if !sliceEqual(got, tc.want) {
				t.Errorf("SplitWords(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitNumbers(t *testing.T) {
	got := tokenize.SplitNumbers("Avery bought 003 books on 2024-05-07.")
	want := []string{"003", "2024", "05", "07"}
	if !sliceEqual(got, want) {
		t.Fatalf("SplitNumbers = %v, want %v", got, want)
	}
	if got := tokenize.SplitNumbers("no digits"); got != nil {
		t.Fatalf("expected nil for text without digits, got %v", got)
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(tokens []string, want string) bool {
	for _, token := range tokens {
		if token == want {
			return true
		}
	}
	return false
}
