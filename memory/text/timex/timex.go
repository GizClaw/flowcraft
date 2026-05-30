// Package timex parses natural-language and absolute time expressions embedded
// inside free text.
//
// The package ships a zero-dependency parser for common calendar shapes,
// lexical relative phrases, durations, and recurring sets. Callers can also
// supply [Parser] implementations to [Extract] when they need broader
// natural-language coverage without making the core package depend on heavier
// parsers.
//
// [Extract] normalizes matches into an [Expression], including the semantic
// kind, precision, range, and TIMEX-like value when available.
package timex

import "time"

// Parser turns a natural-language time expression embedded inside
// text into an absolute timestamp.
//
// Implementations should return a non-nil Match on success and
// (nil, nil) when the text contains no recognisable time
// expression. An error indicates a malformed input that the
// parser refused to attempt — callers should not retry.
//
// now is the reference point used to resolve relative phrases
// ("yesterday", "next Tuesday"). Parsers that handle only
// absolute expressions ignore the argument; passing time.Now()
// is always safe.
type Parser interface {
	Parse(text string, now time.Time) (*Match, error)
}

// Match describes a successfully parsed time expression.
type Match struct {
	// Time is the resolved absolute timestamp. Implementations
	// must set the location to time.UTC when the source text
	// does not specify a timezone, so two parses of the same
	// string compare cleanly.
	Time time.Time
	// Text is the substring of the input that matched the time
	// expression (e.g. "2026-05-20" out of "delivered on
	// 2026-05-20").
	Text string
	// Index is the byte offset where Text begins in the input.
	// Useful for highlighting / span-based downstream tagging.
	Index int
}
