package gse_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/text/tokenize"
	"github.com/GizClaw/flowcraft/sdk/text/tokenize/adapter/gse"
)

// TestNew_DefaultsTokenizeInterface guarantees the adapter satisfies
// tokenize.Tokenizer at compile time. Drift here breaks every
// caller that holds tokenize.Tokenizer references.
func TestNew_DefaultsTokenizeInterface(t *testing.T) {
	tok, err := gse.New()
	if err != nil {
		t.Fatalf("gse.New: %v", err)
	}
	var _ tokenize.Tokenizer = tok
}

func TestTokenize_ChineseRealWords(t *testing.T) {
	tok, err := gse.New()
	if err != nil {
		t.Fatalf("gse.New: %v", err)
	}
	tokens := tok.Tokenize("我喜欢北京天安门")
	if len(tokens) == 0 {
		t.Fatal("expected non-empty Chinese tokens")
	}
	// At least one multi-character word should appear — this is
	// the whole reason to reach for gse over CJKBigram. We assert
	// "北京" / "天安门" exist (the embedded dict knows them).
	found := false
	for _, tok := range tokens {
		if tok == "北京" || tok == "天安门" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected real-word segmentation (北京/天安门) in %v", tokens)
	}
}

func TestTokenize_MixedASCII(t *testing.T) {
	tok, err := gse.New()
	if err != nil {
		t.Fatalf("gse.New: %v", err)
	}
	tokens := tok.Tokenize("Hello 北京 World")
	joined := strings.Join(tokens, " ")
	if !strings.Contains(joined, "hello") {
		t.Errorf("ASCII run should be lowercased by default, got %v", tokens)
	}
}

func TestTokenize_EmptyIsNil(t *testing.T) {
	tok, err := gse.New()
	if err != nil {
		t.Fatalf("gse.New: %v", err)
	}
	if got := tok.Tokenize(""); got != nil {
		t.Errorf("empty input should return nil, got %v", got)
	}
}

// TestTokenize_AlwaysLowercased pins the documented contract: gse
// tokenizer always emits lower-cased output for BM25 consistency.
// Callers needing raw surface forms reach for tokenize.SplitWords.
func TestTokenize_AlwaysLowercased(t *testing.T) {
	tok, err := gse.New()
	if err != nil {
		t.Fatalf("gse.New: %v", err)
	}
	tokens := tok.Tokenize("HELLO WORLD")
	for _, tk := range tokens {
		for _, r := range tk {
			if r >= 'A' && r <= 'Z' {
				t.Errorf("uppercase leaked: token=%q in %v", tk, tokens)
			}
		}
	}
}
