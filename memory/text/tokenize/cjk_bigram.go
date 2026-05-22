package tokenize

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/text/stopword"
)

// CJKBigram handles CJK text by emitting bigrams for CJK runs and
// falling back to [Simple] for non-CJK runs. This avoids a heavy
// external dependency (a real segmenter such as gse) while still
// providing reasonable CJK search recall: a CJK substring of
// length n contributes n unigrams + (n-1) bigrams to the index,
// so any sub-bigram query lights up the document.
//
// Production deployments needing higher CJK precision should plug
// in a real segmenter via a tokenize/adapter sub-package without
// changing call sites — every Tokenizer is interchangeable.
type CJKBigram struct {
	simple Simple
}

// Tokenize implements Tokenizer.
func (t *CJKBigram) Tokenize(text string) []string {
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

// emitCJK walks the CJK rune buffer and emits unigram + bigram
// tokens, skipping any rune classified as a CJK stop character by
// [sdk/text/stopword.IsCJKChar]. The unigram + bigram strategy is
// the documented baseline behaviour of the legacy textsearch
// package and is preserved here byte-for-byte to keep BM25
// vocabulary stable across the migration.
func emitCJK(buf []rune) []string {
	var out []string
	for i := range buf {
		if stopword.IsCJKChar(buf[i]) {
			continue
		}
		out = append(out, string(buf[i]))
		if i+1 < len(buf) && !stopword.IsCJKChar(buf[i+1]) {
			out = append(out, string(buf[i:i+2]))
		}
	}
	return out
}
