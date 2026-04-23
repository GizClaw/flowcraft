package tts

import (
	"strings"
	"unicode/utf8"
)

// SegmenterOption configures a Segmenter.
type SegmenterOption func(*Segmenter)

// EagerMode enables eager segmentation with weak break points and force-break.
func EagerMode() SegmenterOption { return func(s *Segmenter) { s.eager = true } }

// WithMinChars sets the minimum character count before a break point triggers.
func WithMinChars(n int) SegmenterOption { return func(s *Segmenter) { s.minChars = n } }

// WithForceBreakRunes sets the rune count that triggers an eager force-break
// when no punctuation is found. Only applies in eager mode. Default 20.
func WithForceBreakRunes(n int) SegmenterOption { return func(s *Segmenter) { s.forceBreak = n } }

// WithTerminators sets the strong break characters (sentence-ending punctuation).
func WithTerminators(s string) SegmenterOption {
	return func(seg *Segmenter) { seg.terminators = s }
}

// WithWeakBreaks sets the weak break characters (clause-level punctuation,
// used in eager mode only).
func WithWeakBreaks(s string) SegmenterOption {
	return func(seg *Segmenter) { seg.weakBreaks = s }
}

// Segmenter splits a token stream into sentences at punctuation boundaries.
// Not safe for concurrent use.
type Segmenter struct {
	buf         strings.Builder
	minChars    int
	forceBreak  int
	eager       bool
	isFirst     bool
	terminators string
	weakBreaks  string
}

// NewSegmenter creates a new Segmenter.
func NewSegmenter(opts ...SegmenterOption) *Segmenter {
	s := &Segmenter{
		minChars:    8,
		forceBreak:  20,
		isFirst:     true,
		terminators: defaultTerminators,
		weakBreaks:  defaultWeakBreaks,
	}
	for _, o := range opts {
		o(s)
	}
	if s.minChars < 1 {
		s.minChars = 1
	}
	if s.forceBreak < s.minChars {
		s.forceBreak = s.minChars * 2
	}
	return s
}

// Feed receives a token and may return a complete sentence.
func (s *Segmenter) Feed(token string) (sentence string, ok bool) {
	s.buf.WriteString(token)
	text := s.buf.String()

	if byteIdx := s.findBreakPoint(text); byteIdx >= 0 {
		sentence = text[:byteIdx+1]
		s.buf.Reset()
		s.buf.WriteString(text[byteIdx+1:])
		s.isFirst = false
		return strings.TrimSpace(sentence), true
	}

	if s.eager && utf8.RuneCountInString(text) >= s.forceBreak {
		sentence = text
		s.buf.Reset()
		s.isFirst = false
		return strings.TrimSpace(sentence), true
	}

	return "", false
}

// Flush returns any remaining buffered text.
func (s *Segmenter) Flush() string {
	text := strings.TrimSpace(s.buf.String())
	s.buf.Reset()
	return text
}

const (
	defaultTerminators = "。！？.!?\n"
	defaultWeakBreaks  = "，、,;：:）)"
)

func (s *Segmenter) findBreakPoint(text string) int {
	runeCount := utf8.RuneCountInString(text)
	min := s.minChars
	if s.isFirst && s.eager {
		min = 4
	}
	if runeCount < min {
		return -1
	}

	if idx := s.scanBackwards(text, s.terminators, min); idx >= 0 {
		return idx
	}

	if s.eager {
		if idx := s.scanBackwards(text, s.weakBreaks, min); idx >= 0 {
			return idx
		}
	}

	return -1
}

func (s *Segmenter) scanBackwards(text string, charset string, minRunes int) int {
	runeIdx := utf8.RuneCountInString(text)
	bytePos := len(text)

	for bytePos > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:bytePos])
		runeIdx--
		bytePos -= size

		if runeIdx < minRunes-1 {
			break
		}

		if strings.ContainsRune(charset, r) {
			return bytePos + size - 1
		}
	}
	return -1
}
