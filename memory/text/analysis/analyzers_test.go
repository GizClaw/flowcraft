package analysis_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/text/analysis"
)

func TestSimpleAnalyzerEnglishPipeline(t *testing.T) {
	analyzer := &analysis.SimpleAnalyzer{}
	tokens := analyzer.Analyze("The HELLO went running programs", analysis.Options{Mode: analysis.ModeIndex})
	terms := analysis.Terms(tokens)
	want := []string{"hello", "went", "run", "program"}
	if !sliceEqual(terms, want) {
		t.Fatalf("terms = %v, want %v", terms, want)
	}
}

func TestSimpleAnalyzerStopwordPositionIncr(t *testing.T) {
	analyzer := &analysis.SimpleAnalyzer{}
	tokens := analyzer.Analyze("the quick brown", analysis.Options{Mode: analysis.ModeIndex})
	if len(tokens) < 1 {
		t.Fatal("expected tokens")
	}
	if tokens[0].Term != "quick" || tokens[0].Position != 1 || tokens[0].PositionIncr != 2 {
		t.Fatalf("first token = %+v, want quick at position 1 with incr 2", tokens[0])
	}
}

func TestCJKBigramAnalyzerEmitsUnigramsAndBigrams(t *testing.T) {
	analyzer := &analysis.CJKBigramAnalyzer{}
	tokens := analyzer.Analyze("知识检索 hello", analysis.Options{Mode: analysis.ModeIndex})
	terms := analysis.Terms(tokens)
	want := []string{"知", "知识", "识", "识检", "检", "检索", "索", "hello"}
	if !sliceEqual(terms, want) {
		t.Fatalf("terms = %v, want %v", terms, want)
	}
	if tokens[0].Type != analysis.Ideographic || tokens[1].Type != analysis.Shingle {
		t.Fatalf("unexpected token types: %+v %+v", tokens[0], tokens[1])
	}
	if tokens[1].Position != 0 || tokens[1].PositionIncr != 0 || tokens[1].PositionLength != 2 {
		t.Fatalf("bigram token position = %+v, want same-position shingle length 2", tokens[1])
	}
}

func TestCJKBigramAnalyzerKeepsBleveCJKTokens(t *testing.T) {
	analyzer := &analysis.CJKBigramAnalyzer{}
	tokens := analyzer.Analyze("知识的检索", analysis.Options{Mode: analysis.ModeIndex})
	terms := analysis.Terms(tokens)
	for _, want := range []string{"知", "知识", "识", "识的", "的", "的检", "检", "检索", "索"} {
		if !contains(terms, want) {
			t.Fatalf("expected %q in Bleve CJK terms, got %v", want, terms)
		}
	}
}

func TestDetect(t *testing.T) {
	if _, ok := analysis.Detect("hello world").(*analysis.SimpleAnalyzer); !ok {
		t.Fatal("expected SimpleAnalyzer for ASCII text")
	}
	if _, ok := analysis.Detect("这是中文内容").(*analysis.CJKBigramAnalyzer); !ok {
		t.Fatal("expected CJKBigramAnalyzer for CJK text")
	}
}

func TestOffsetsAndPositions(t *testing.T) {
	analyzer := &analysis.CJKBigramAnalyzer{}
	tokens := analyzer.Analyze("Hi 世界", analysis.Options{Mode: analysis.ModeIndex, NeedOffset: true, NeedType: true})
	if len(tokens) < 4 {
		t.Fatalf("expected at least 4 tokens, got %v", tokens)
	}
	if tokens[0].Term != "hi" || tokens[0].Start != 0 || tokens[0].End != 2 || tokens[0].Position != 0 {
		t.Fatalf("latin token = %+v, want hi [0,2) at position 0", tokens[0])
	}
	if tokens[1].Term != "世" || tokens[1].Start != 3 || tokens[1].End != 6 || tokens[1].Position != 1 {
		t.Fatalf("cjk unigram = %+v, want [3,6) at position 1", tokens[1])
	}
	if tokens[2].Term != "世界" || tokens[2].Start != 3 || tokens[2].End != 9 || tokens[2].PositionIncr != 0 {
		t.Fatalf("cjk bigram = %+v, want [3,9) at same position", tokens[2])
	}
}

func TestExtractKeywordsDedupesInTokenOrder(t *testing.T) {
	analyzer := &analysis.SimpleAnalyzer{}
	got := analysis.ExtractKeywords("Hello world hello test world", analyzer)
	want := []string{"hello", "world", "test"}
	if !sliceEqual(got, want) {
		t.Fatalf("ExtractKeywords = %v, want %v", got, want)
	}
}

func TestSimpleAnalyzerLanguageOptionUsesBleveAnalyzer(t *testing.T) {
	analyzer := &analysis.SimpleAnalyzer{}
	tokens := analysis.Terms(analyzer.Analyze("les options rapides", analysis.Options{
		Mode:     analysis.ModeIndex,
		Language: "french",
	}))
	if contains(tokens, "les") {
		t.Fatalf("expected French stopword to be filtered, got %v", tokens)
	}
	if !contains(tokens, "option") || !contains(tokens, "rapid") {
		t.Fatalf("expected Bleve French stems in tokens, got %v", tokens)
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
