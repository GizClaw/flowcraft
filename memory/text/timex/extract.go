package timex

import "time"

// MatchSource describes which extractor produced an expression.
type MatchSource string

const (
	MatchSourceCalendar MatchSource = "calendar"
	MatchSourceNatural  MatchSource = "natural"
	MatchSourceRelative MatchSource = "relative"
)

// Expression is the unified result of deterministic calendar parsing, optional
// natural-language parsers, and lexical relative-time detection.
type Expression struct {
	Match
	Source               MatchSource
	Precision            CalendarPrecision
	HasCalendarPrecision bool
	Relative             bool
}

// Extract returns the first time expression found in text. Locale-independent
// numeric dates run first, then supplied natural-language parsers, then the
// built-in word-calendar parser, and finally lexical relative-time fallback.
func Extract(text string, anchor time.Time, parsers ...Parser) (*Expression, error) {
	if text == "" {
		return nil, nil
	}
	if anchor.IsZero() {
		anchor = time.Now().UTC()
	}
	if cal := ParseNumericCalendar(text); cal != nil {
		return &Expression{
			Match:                Match{Time: cal.Time, Text: cal.Text, Index: cal.Index},
			Source:               MatchSourceCalendar,
			Precision:            cal.Precision,
			HasCalendarPrecision: true,
			Relative:             false,
		}, nil
	}
	var firstErr error
	for _, parser := range parsers {
		if parser == nil {
			continue
		}
		m, err := parser.Parse(text, anchor)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if m == nil {
			continue
		}
		expr := &Expression{
			Match:    *m,
			Source:   MatchSourceNatural,
			Relative: IsRelativePhrase(m.Text),
		}
		if cal := ParseCalendar(m.Text); cal != nil {
			expr.Precision = cal.Precision
			expr.HasCalendarPrecision = true
			expr.Relative = false
		}
		return expr, nil
	}
	if cal := parseCalendarWords(text); cal != nil {
		return &Expression{
			Match:                Match{Time: cal.Time, Text: cal.Text, Index: cal.Index},
			Source:               MatchSourceCalendar,
			Precision:            cal.Precision,
			HasCalendarPrecision: true,
			Relative:             false,
		}, nil
	}
	if rel := FindRelativePhrase(text); rel != nil {
		t, ok := resolveLexicalRelative(rel.Text, anchor)
		if !ok {
			t = time.Time{}
		}
		return &Expression{
			Match:    Match{Time: t, Text: rel.Text, Index: rel.Index},
			Source:   MatchSourceRelative,
			Relative: true,
		}, nil
	}
	return nil, firstErr
}
