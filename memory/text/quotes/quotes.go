// Package quotes provides quote-span extraction over text that mixes
// ASCII, smart, and CJK quotation marks.
//
// The package answers one question — "what spans of text were
// explicitly quoted?" — and is the right primitive for callers that
// need to lift caller-emphasised entities out of a free-form input
// (NER hint extraction, evidence quoting, prompt safety filters)
// without baking quote-character knowledge into every consumer.
//
// Supported quote characters:
//
//   - ASCII straight quote     U+0022  "
//   - LEFT  DOUBLE QUOTATION   U+201C  "
//   - RIGHT DOUBLE QUOTATION   U+201D  "
//   - LEFT  CORNER BRACKET     U+300C  「
//   - RIGHT CORNER BRACKET     U+300D  」
//
// All quote characters are treated as a single "any quote opens or
// closes a span" set so the extractor stays tolerant of input that
// mixes typographic conventions (macOS autocorrect smart quotes
// inside a JSON payload, CJK corner brackets inside an English
// sentence, etc.). Callers who need strict ASCII-only behaviour
// should pre-filter the input.
//
// Span pairing is order-preserving and non-nesting: the first quote
// opens, the next quote (regardless of glyph) closes, and so on. An
// unclosed final span is dropped (returning a half-open span risks
// surfacing trailing prose that is not actually quoted).
package quotes

import "strings"

// quoteRunes is the closed set of recognised quote characters.
// Lookups happen on the hot path so a small switch outperforms a map.
func isQuote(r rune) bool {
	switch r {
	case '"', '\u201c', '\u201d', '\u300c', '\u300d':
		return true
	}
	return false
}

// ExtractSpans returns the content of every quoted span in s, in
// source order. The returned slice is nil when the input contains no
// recognised quotes. Empty quoted spans ("") are dropped because the
// caller's NER / evidence consumers have no signal to extract from
// them.
//
// Pairing is non-nesting: the first quote opens, the next quote
// (regardless of glyph) closes. An unclosed final span is dropped so
// callers never see trailing prose mis-attributed as quoted.
func ExtractSpans(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	in := false
	for _, r := range s {
		if isQuote(r) {
			if in {
				if cur.Len() > 0 {
					out = append(out, cur.String())
				}
				cur.Reset()
				in = false
			} else {
				in = true
			}
			continue
		}
		if in {
			cur.WriteRune(r)
		}
	}
	return out
}
