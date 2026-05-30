package timex

import "time"

// MatchSource describes which extractor produced an expression.
type MatchSource string

const (
	MatchSourceCalendar MatchSource = "calendar"
	MatchSourceNatural  MatchSource = "natural"
	MatchSourceRelative MatchSource = "relative"
	MatchSourceDuration MatchSource = "duration"
	MatchSourceSet      MatchSource = "set"
)

// ExpressionKind describes the normalized semantic shape of a time expression.
type ExpressionKind string

const (
	ExpressionKindDate      ExpressionKind = "date"
	ExpressionKindDateRange ExpressionKind = "daterange"
	ExpressionKindDuration  ExpressionKind = "duration"
	ExpressionKindSet       ExpressionKind = "set"
)

// Expression is the unified result of deterministic calendar parsing, optional
// natural-language parsers, and lexical relative-time detection.
type Expression struct {
	Match
	Source               MatchSource
	Kind                 ExpressionKind
	Timex                string
	Precision            CalendarPrecision
	HasCalendarPrecision bool
	HasPrecision         bool
	Relative             bool
	Start                time.Time
	End                  time.Time
	HasRange             bool
}

// Extract returns the first time expression found in text. Multiple grammar
// families are allowed to produce candidates; the earliest span wins, and ties
// prefer the more specific normalization path.
func Extract(text string, anchor time.Time, parsers ...Parser) (*Expression, error) {
	if text == "" {
		return nil, nil
	}
	if anchor.IsZero() {
		anchor = time.Now().UTC()
	}
	candidates, err := runRecognizers(
		text,
		anchor,
		numericCalendarRecognizer{},
		durationRecognizer{},
		setRecognizer{},
		parserRecognizer{parsers: parsers},
		calendarRangeRecognizer{},
		calendarWordsRecognizer{},
		relativeRecognizer{},
	)
	if selected, ok := selectFirstCandidate(candidates); ok {
		return expressionFromCandidate(selected), nil
	}
	return nil, err
}
