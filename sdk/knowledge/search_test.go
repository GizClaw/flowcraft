package knowledge

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/textsearch"
)

func TestCorpusStats_AddRemove(t *testing.T) {
	cs := NewCorpusStats()
	tokens1 := []string{"hello", "world", "go"}
	tokens2 := []string{"hello", "test"}

	cs.AddDocument(tokens1)
	if cs.DocCount != 1 {
		t.Fatalf("expected 1 doc, got %d", cs.DocCount)
	}

	cs.AddDocument(tokens2)
	if cs.DocCount != 2 {
		t.Fatalf("expected 2 docs, got %d", cs.DocCount)
	}
	if cs.DocFreq["hello"] != 2 {
		t.Fatalf("expected hello df=2, got %d", cs.DocFreq["hello"])
	}

	cs.RemoveDocument(tokens1)
	if cs.DocCount != 1 {
		t.Fatalf("expected 1 doc after remove, got %d", cs.DocCount)
	}
	if cs.DocFreq["hello"] != 1 {
		t.Fatalf("expected hello df=1 after remove, got %d", cs.DocFreq["hello"])
	}
	if cs.DocFreq["world"] != 0 {
		t.Fatalf("expected world df=0, got %d", cs.DocFreq["world"])
	}
}

func TestScoreChunk_Match(t *testing.T) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()
	cs.AddDocument(tok.Tokenize("Go programming language"))
	cs.AddDocument(tok.Tokenize("Python scripting"))

	chunk := &Chunk{Content: "Go programming language is great"}
	score := ScoreChunk(chunk, ExtractKeywords("go programming", tok), cs, tok)
	if score <= 0 {
		t.Fatalf("expected positive score, got %f", score)
	}
}

func TestScoreChunk_NoMatch(t *testing.T) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()
	cs.AddDocument(tok.Tokenize("Go programming"))

	chunk := &Chunk{Content: "Go programming"}
	score := ScoreChunk(chunk, ExtractKeywords("python", tok), cs, tok)
	if score != 0 {
		t.Fatalf("expected 0, got %f", score)
	}
}

func TestScoreChunk_NilCorpus(t *testing.T) {
	tok := &SimpleTokenizer{}
	chunk := &Chunk{Content: "test"}
	score := ScoreChunk(chunk, []string{"test"}, nil, tok)
	if score != 0 {
		t.Fatalf("expected 0 for nil corpus, got %f", score)
	}
}

func TestExtractKeywords(t *testing.T) {
	tok := &SimpleTokenizer{}
	kws := ExtractKeywords("Hello world hello test", tok)
	seen := make(map[string]int)
	for _, kw := range kws {
		seen[kw]++
	}
	for _, kw := range kws {
		if seen[kw] > 1 {
			t.Fatalf("keyword %q appears %d times (not deduped)", kw, seen[kw])
		}
	}
	if len(kws) == 0 {
		t.Fatal("expected non-empty keywords")
	}
}

func TestRankResults(t *testing.T) {
	results := []SearchResult{
		{Score: 1.0}, {Score: 3.0}, {Score: 2.0},
	}
	ranked := RankResults(results, 2)
	if len(ranked) != 2 {
		t.Fatalf("expected 2, got %d", len(ranked))
	}
	if ranked[0].Score != 3.0 {
		t.Fatalf("expected highest score first, got %f", ranked[0].Score)
	}
}

func TestParseFrontmatter(t *testing.T) {
	raw := "---\ntitle: Test\ntags: api, ref\n---\n# Content\nBody here"
	body, meta := parseFrontmatter(raw)
	if meta["title"] != "Test" {
		t.Fatalf("expected title=Test, got %q", meta["title"])
	}
	if body != "# Content\nBody here" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	raw := "# Just content"
	body, meta := parseFrontmatter(raw)
	if meta != nil {
		t.Fatal("expected nil meta")
	}
	if body != raw {
		t.Fatalf("expected full content as body")
	}
}

func toAnyMap(m map[string]string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestToAnyMap(t *testing.T) {
	m := toAnyMap(map[string]string{"k": "v"})
	if m["k"] != "v" {
		t.Fatal("expected v")
	}
	if toAnyMap(nil) != nil {
		t.Fatal("expected nil")
	}
}

func TestBM25_RankingCorrectness(t *testing.T) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()

	doc1Tokens := tok.Tokenize("Go programming language concurrency goroutines channels")
	doc2Tokens := tok.Tokenize("Python scripting language dynamic typing")
	doc3Tokens := tok.Tokenize("Go Go Go programming programming language language concurrency concurrency")

	cs.AddDocument(doc1Tokens)
	cs.AddDocument(doc2Tokens)
	cs.AddDocument(doc3Tokens)

	chunk1 := &Chunk{Content: "Go programming language concurrency goroutines channels"}
	chunk3 := &Chunk{Content: "Go Go Go programming programming language language concurrency concurrency"}
	chunkPython := &Chunk{Content: "Python scripting language dynamic typing"}

	keywords := ExtractKeywords("Go programming", tok)

	score1 := ScoreChunk(chunk1, keywords, cs, tok)
	score3 := ScoreChunk(chunk3, keywords, cs, tok)
	scorePy := ScoreChunk(chunkPython, keywords, cs, tok)

	if score1 <= 0 {
		t.Fatalf("expected positive score for Go doc, got %f", score1)
	}
	if score3 <= 0 {
		t.Fatalf("expected positive score for Go-heavy doc, got %f", score3)
	}
	if scorePy >= score1 {
		t.Fatalf("Python doc (%f) should score lower than Go doc (%f) for 'Go programming'", scorePy, score1)
	}
	if score3 <= score1 {
		t.Fatalf("doc with higher TF for query terms (%f) should score higher than doc1 (%f)", score3, score1)
	}
}

func TestBM25_IDFWeighting(t *testing.T) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()

	for i := 0; i < 10; i++ {
		cs.AddDocument(tok.Tokenize("common word appears everywhere"))
	}
	cs.AddDocument(tok.Tokenize("rare unique term special"))

	chunkWithRare := &Chunk{Content: "this document has rare unique term and common word"}
	chunkCommonOnly := &Chunk{Content: "this document has common word appears everywhere"}

	scoreRare := ScoreChunk(chunkWithRare, ExtractKeywords("rare unique", tok), cs, tok)
	scoreCommon := ScoreChunk(chunkCommonOnly, ExtractKeywords("common word", tok), cs, tok)

	if scoreRare <= 0 {
		t.Fatalf("expected positive score for rare term match, got %f", scoreRare)
	}
	if scoreCommon <= 0 {
		t.Fatalf("expected positive score for common term match, got %f", scoreCommon)
	}
	if scoreRare <= scoreCommon {
		t.Fatalf("rare terms (%f) should yield higher IDF and thus higher score than common terms (%f)", scoreRare, scoreCommon)
	}
}

func TestStemming_RecallImprovement(t *testing.T) {
	tok := &SimpleTokenizer{}

	cs := NewCorpusStats()
	docText := "programming languages support concurrent computing"
	cs.AddDocument(tok.Tokenize(docText))

	chunk := &Chunk{Content: docText}

	// "programs" stems to "program", "programming" also stems to "program" -> match
	score := ScoreChunk(chunk, ExtractKeywords("programs language", tok), cs, tok)
	if score <= 0 {
		t.Fatalf("stemming should enable 'programs' to match 'programming', got score %f", score)
	}

	// Verify the stem equivalence directly
	if textsearch.Stem("programs") != textsearch.Stem("programming") {
		t.Fatalf("expected Stem('programs')=%q == Stem('programming')=%q", textsearch.Stem("programs"), textsearch.Stem("programming"))
	}
	if textsearch.Stem("languages") != textsearch.Stem("language") {
		t.Fatalf("expected Stem('languages')=%q == Stem('language')=%q", textsearch.Stem("languages"), textsearch.Stem("language"))
	}

	// Without matching stems, score should be 0
	scoreNoMatch := ScoreChunk(chunk, ExtractKeywords("database storage", tok), cs, tok)
	if scoreNoMatch != 0 {
		t.Fatalf("expected 0 score for unrelated query, got %f", scoreNoMatch)
	}
}

func TestBM25_LengthNormalization(t *testing.T) {
	tok := &SimpleTokenizer{}
	cs := NewCorpusStats()

	short := "Go concurrency patterns"
	long := "Go concurrency patterns " + strings.Repeat("filler text padding words ", 50)

	cs.AddDocument(tok.Tokenize(short))
	cs.AddDocument(tok.Tokenize(long))

	keywords := ExtractKeywords("Go concurrency", tok)
	scoreShort := ScoreText(short, keywords, cs, tok)
	scoreLong := ScoreText(long, keywords, cs, tok)

	if scoreShort <= scoreLong {
		t.Errorf("short doc should score higher due to length normalization (b=0.75): short=%f, long=%f",
			scoreShort, scoreLong)
	}
}
