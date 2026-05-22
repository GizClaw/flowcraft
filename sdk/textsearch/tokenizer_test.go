package textsearch

import "testing"

func TestSimpleTokenizer(t *testing.T) {
	tok := &SimpleTokenizer{}
	tokens := tok.Tokenize("Hello World programming test")
	if len(tokens) == 0 {
		t.Fatal("expected tokens")
	}
	for _, tk := range tokens {
		if stopWords[tk] {
			t.Fatalf("stop word %q should have been filtered", tk)
		}
	}
}

func TestSimpleTokenizer_StopWords(t *testing.T) {
	tok := &SimpleTokenizer{}
	tokens := tok.Tokenize("the a an is are to in for")
	if len(tokens) != 0 {
		t.Fatalf("expected 0 tokens (all stop words), got %d: %v", len(tokens), tokens)
	}
}

func TestSimpleTokenizer_ShortTokens(t *testing.T) {
	tok := &SimpleTokenizer{}
	tokens := tok.Tokenize("a b c d go")
	for _, tk := range tokens {
		if len(tk) < 2 {
			t.Fatalf("token %q is too short", tk)
		}
	}
}

func TestCJKTokenizer_Chinese(t *testing.T) {
	tok := &CJKTokenizer{}
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

func TestDetectTokenizer_ASCII(t *testing.T) {
	tok := DetectTokenizer("hello world test")
	if _, ok := tok.(*SimpleTokenizer); !ok {
		t.Fatal("expected SimpleTokenizer for ASCII text")
	}
}

func TestDetectTokenizer_CJK(t *testing.T) {
	tok := DetectTokenizer("这是中文内容")
	if _, ok := tok.(*CJKTokenizer); !ok {
		t.Fatal("expected CJKTokenizer for CJK text")
	}
}

func TestIsCJK(t *testing.T) {
	if !IsCJK('你') {
		t.Fatal("expected '你' to be CJK")
	}
	if IsCJK('a') {
		t.Fatal("expected 'a' to not be CJK")
	}
}

func TestCJKTokenizer_StopChars(t *testing.T) {
	tok := &CJKTokenizer{}
	tokens := tok.Tokenize("的了在是")
	if len(tokens) != 0 {
		t.Fatalf("expected 0 tokens (all CJK stop chars), got %d: %v", len(tokens), tokens)
	}
}

func TestCJKTokenizer_MixedWithStopChars(t *testing.T) {
	tok := &CJKTokenizer{}
	tokens := tok.Tokenize("知识的检索")
	for _, tk := range tokens {
		for _, r := range tk {
			if IsCJKStopChar(r) {
				t.Fatalf("token %q contains CJK stop char", tk)
			}
		}
	}
	if len(tokens) == 0 {
		t.Fatal("expected non-empty tokens after filtering CJK stop chars")
	}
}

func TestSimpleTokenizer_ExpandedStopWords(t *testing.T) {
	tok := &SimpleTokenizer{}
	expanded := []string{"but", "if", "so", "about", "because", "while", "during", "between", "also", "very"}
	for _, sw := range expanded {
		tokens := tok.Tokenize(sw)
		if len(tokens) != 0 {
			t.Fatalf("expected %q to be filtered as stop word, got %v", sw, tokens)
		}
	}
}
