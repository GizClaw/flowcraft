// Package textsearch's shim_test pins the deprecated public surface
// area so it cannot quietly drift from the sdk/text/* targets.
//
// It does NOT re-verify algorithm correctness (that lives in
// sdk/text/{tokenize,stopword,stem,lemma,bm25}); it only checks:
//
//   - every deprecated symbol still compiles
//   - alias types are interchangeable with their text/* targets
//     via type assertions (so callers can `var _ X = (*Y)(nil)`
//     across the shim boundary)
//   - wrapper functions forward to the new symbol — one call each,
//     verifying the side-effect is the same as a direct text/*
//     call
//
// symbols on purpose; staticcheck noise here would mask real
// external-consumer warnings.
//
//lint:file-ignore SA1019 the file's job is to call the deprecated
package textsearch_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/text/bm25"
	"github.com/GizClaw/flowcraft/sdk/text/lemma"
	"github.com/GizClaw/flowcraft/sdk/text/stem"
	"github.com/GizClaw/flowcraft/sdk/text/stopword"
	"github.com/GizClaw/flowcraft/sdk/text/tokenize"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
)

// Compile-time alias guards. If any of these fails to type-check,
// the shim has drifted from text/* and the migration plan is
// broken — every external consumer would break with it.
var (
	_ textsearch.Tokenizer    = (*textsearch.SimpleTokenizer)(nil)
	_ textsearch.Tokenizer    = (*textsearch.CJKTokenizer)(nil)
	_ tokenize.Tokenizer      = (*textsearch.SimpleTokenizer)(nil)
	_ tokenize.Tokenizer      = (*textsearch.CJKTokenizer)(nil)
	_ *textsearch.CorpusStats = (*bm25.CorpusStats)(nil)
	_ textsearch.ScoreOption  = bm25.ScoreOption(nil)
)

func TestShim_TokenizerAliases(t *testing.T) {
	if _, ok := textsearch.DetectTokenizer("hello").(*textsearch.SimpleTokenizer); !ok {
		t.Error("Detect on ASCII must return *SimpleTokenizer alias")
	}
	if _, ok := textsearch.DetectTokenizer("你好").(*textsearch.CJKTokenizer); !ok {
		t.Error("Detect on CJK must return *CJKTokenizer alias")
	}
}

func TestShim_StopWordWrappers(t *testing.T) {
	if !textsearch.IsStopWord("the") {
		t.Error("IsStopWord must forward to stopword.IsEnglish")
	}
	if textsearch.IsStopWord("alice") {
		t.Error("IsStopWord must not flag proper nouns")
	}
	if !textsearch.IsCJKStopChar('的') {
		t.Error("IsCJKStopChar must forward to stopword.IsCJKChar")
	}
	if !textsearch.IsCJK('你') {
		t.Error("IsCJK must forward to tokenize.IsCJK")
	}
}

func TestShim_StemAndLemma(t *testing.T) {
	if got, want := textsearch.Stem("running"), stem.Porter("running"); got != want {
		t.Errorf("Stem wrapper forward mismatch: %q vs %q", got, want)
	}
	if got, want := textsearch.Lemmatize("went"), lemma.Lemmatize("went"); got != want {
		t.Errorf("Lemmatize wrapper forward mismatch: %q vs %q", got, want)
	}
}

func TestShim_BM25Wrappers(t *testing.T) {
	cs := textsearch.NewCorpusStats()
	tok := &textsearch.SimpleTokenizer{}

	cs.AddDocument(tok.Tokenize("Go programming language"))
	cs.AddDocument(tok.Tokenize("Python scripting"))

	kw := textsearch.ExtractKeywords("Go programming", tok)
	if len(kw) == 0 {
		t.Fatal("ExtractKeywords wrapper returned empty slice")
	}

	// shim wrapper vs direct text/bm25 call must match byte-for-byte:
	// same corpus, same tokens, same options.
	got := textsearch.ScoreText("Go programming is great", kw, cs, tok)
	want := bm25.ScoreText("Go programming is great", kw, cs, tok)
	if got != want {
		t.Errorf("ScoreText shim drift: shim=%f text/bm25=%f", got, want)
	}

	docTokens := tok.Tokenize("Go programming")
	got = textsearch.BM25(docTokens, kw, cs, textsearch.WithK1(2.0), textsearch.WithB(0.5))
	want = bm25.Score(docTokens, kw, cs, bm25.WithK1(2.0), bm25.WithB(0.5))
	if got != want {
		t.Errorf("BM25 shim drift (with options): shim=%f text/bm25=%f", got, want)
	}
}

// TestShim_StopwordSetExtensionPoint is an end-to-end smoke test for
// callers who built on top of the shim and want to confirm the
// stopword.Set extension point is now reachable for production-
// grade stop-word management. It does NOT live in the stopword
// package because we want a single touch-test from the migration
// shim's perspective.
func TestShim_StopwordSetExtensionPoint(t *testing.T) {
	custom := stopword.EnglishSet().Extend("matcha", "memory")
	if !custom.Contains("the") {
		t.Error("custom Set must inherit baseline entries")
	}
	if !custom.Contains("Matcha") {
		t.Error("custom Set must contain extended (case-insensitive) entry")
	}
	if stopword.IsEnglish("matcha") {
		t.Error("package baseline must NOT change when caller extends a copy")
	}
}
