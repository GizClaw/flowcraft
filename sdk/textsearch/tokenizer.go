// Package textsearch provides lightweight BM25 text search primitives:
// tokenizers (ASCII + CJK), Porter stemming, corpus statistics, and scoring.
package textsearch

import (
	"strings"
	"unicode"
)

// Tokenizer splits text into searchable tokens.
type Tokenizer interface {
	Tokenize(text string) []string
}

// SimpleTokenizer splits on whitespace and punctuation, removes stop words
// and tokens shorter than 2 characters.
type SimpleTokenizer struct{}

func (t *SimpleTokenizer) Tokenize(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var out []string
	for _, w := range words {
		if len(w) < 2 || stopWords[w] {
			continue
		}
		out = append(out, Stem(w))
	}
	return out
}

// CJKTokenizer handles CJK text by emitting bigrams for CJK runs and
// falling back to SimpleTokenizer for non-CJK text. This avoids a heavy
// external dependency (gse) while still providing reasonable CJK search.
type CJKTokenizer struct {
	simple SimpleTokenizer
}

func (t *CJKTokenizer) Tokenize(text string) []string {
	text = strings.ToLower(text)
	var tokens []string
	var cjkBuf []rune
	var asciiBuf strings.Builder

	flush := func() {
		tokens = append(tokens, emitCJK(cjkBuf)...)
		cjkBuf = cjkBuf[:0]
		if asciiBuf.Len() > 0 {
			tokens = append(tokens, t.simple.Tokenize(asciiBuf.String())...)
			asciiBuf.Reset()
		}
	}

	for _, r := range text {
		if IsCJK(r) {
			if asciiBuf.Len() > 0 {
				tokens = append(tokens, t.simple.Tokenize(asciiBuf.String())...)
				asciiBuf.Reset()
			}
			cjkBuf = append(cjkBuf, r)
		} else {
			if len(cjkBuf) > 0 {
				tokens = append(tokens, emitCJK(cjkBuf)...)
				cjkBuf = cjkBuf[:0]
			}
			asciiBuf.WriteRune(r)
		}
	}
	flush()
	return tokens
}

func emitCJK(buf []rune) []string {
	var out []string
	for i := range buf {
		if cjkStopChars[buf[i]] {
			continue
		}
		out = append(out, string(buf[i]))
		if i+1 < len(buf) && !cjkStopChars[buf[i+1]] {
			out = append(out, string(buf[i:i+2]))
		}
	}
	return out
}

// IsCJK reports whether r is a CJK character (Han, Hangul, Katakana, Hiragana).
func IsCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hiragana, r)
}

// DetectTokenizer returns a CJKTokenizer if text contains CJK characters,
// otherwise a SimpleTokenizer.
func DetectTokenizer(sampleText string) Tokenizer {
	for _, r := range sampleText {
		if IsCJK(r) {
			return &CJKTokenizer{}
		}
	}
	return &SimpleTokenizer{}
}

var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "do": true, "does": true,
	"did": true, "will": true, "would": true, "could": true, "should": true,
	"of": true, "to": true, "in": true, "for": true, "on": true,
	"with": true, "at": true, "by": true, "from": true, "it": true,
	"this": true, "that": true, "and": true, "or": true, "not": true,
	"i": true, "me": true, "my": true, "we": true, "our": true,
	"you": true, "your": true, "he": true, "she": true, "they": true,
	"but": true, "if": true, "so": true, "no": true, "up": true,
	"out": true, "about": true, "into": true, "than": true, "then": true,
	"its": true, "his": true, "her": true, "their": true, "them": true,
	"him": true, "us": true, "who": true, "which": true, "what": true,
	"when": true, "where": true, "how": true, "all": true, "each": true,
	"every": true, "both": true, "few": true, "more": true, "most": true,
	"other": true, "some": true, "such": true, "only": true, "own": true,
	"same": true, "just": true, "because": true, "as": true, "until": true,
	"while": true, "during": true, "before": true, "after": true, "above": true,
	"below": true, "between": true, "under": true, "again": true, "further": true,
	"once": true, "here": true, "there": true, "any": true, "can": true,
	"also": true, "may": true, "shall": true, "might": true, "must": true,
	"need": true, "very": true, "too": true, "these": true, "those": true,
}

var cjkStopChars = map[rune]bool{
	'的': true, '了': true, '在': true, '是': true, '我': true,
	'有': true, '和': true, '就': true, '不': true, '人': true,
	'都': true, '一': true, '个': true, '上': true, '也': true,
	'很': true, '到': true, '说': true, '要': true, '去': true,
	'你': true, '会': true, '着': true, '没': true, '看': true,
	'好': true, '自': true, '这': true, '他': true, '她': true,
	'它': true, '们': true, '那': true, '被': true, '从': true,
	'把': true, '让': true, '给': true, '向': true, '吧': true,
	'吗': true, '呢': true, '啊': true, '哦': true, '嗯': true,
	'呀': true, '啦': true, '哈': true, '嘛': true, '么': true,
}

// IsStopWord reports whether word is filtered as a stop word by SimpleTokenizer.
func IsStopWord(word string) bool {
	return stopWords[strings.ToLower(word)]
}

// IsCJKStopChar reports whether r is filtered as a stop character by CJKTokenizer.
func IsCJKStopChar(r rune) bool {
	return cjkStopChars[r]
}
