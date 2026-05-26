package timex

import (
	"fmt"
	"time"
)

func expressionFromCandidate(candidate temporalCandidate) *Expression {
	return &Expression{
		Match: Match{
			Time:  candidate.value.Time,
			Text:  candidate.span.Text,
			Index: candidate.span.Index,
		},
		Source:               candidate.source,
		Kind:                 candidate.value.Kind,
		Timex:                candidate.value.Timex,
		Precision:            candidate.value.Precision,
		HasCalendarPrecision: candidate.hasCalendarPrecision,
		HasPrecision:         candidate.value.HasPrecision,
		Relative:             candidate.value.Relative,
		Start:                candidate.value.Start,
		End:                  candidate.value.End,
		HasRange:             candidate.value.HasRange,
	}
}

func candidateFromCalendar(cal *CalendarMatch, source MatchSource, priority int) temporalCandidate {
	return temporalCandidate{
		span:                 temporalSpan{Text: cal.Text, Index: cal.Index},
		source:               source,
		value:                calendarValue(cal.Time, cal.Precision),
		hasCalendarPrecision: true,
		priority:             priority,
	}
}

func candidateFromCalendarRange(match *calendarRangeMatch) temporalCandidate {
	return temporalCandidate{
		span:   temporalSpan{Text: match.Text, Index: match.Index},
		source: MatchSourceCalendar,
		value: temporalValue{
			Kind:         ExpressionKindDateRange,
			Timex:        match.Timex,
			Time:         match.Start,
			Precision:    match.Precision,
			HasPrecision: true,
			Start:        match.Start,
			End:          match.End,
			HasRange:     true,
		},
		hasCalendarPrecision: true,
		priority:             candidatePriorityCalendarRange,
	}
}

func candidateFromNaturalMatch(m *Match, anchor time.Time) temporalCandidate {
	candidate := temporalCandidate{
		span:     temporalSpan{Text: m.Text, Index: m.Index},
		source:   MatchSourceNatural,
		value:    temporalValue{Time: m.Time, Relative: IsRelativePhrase(m.Text)},
		priority: candidatePriorityNatural,
	}
	if cal := ParseCalendar(m.Text); cal != nil {
		candidate.value = calendarValue(cal.Time, cal.Precision)
		candidate.value.Relative = false
		candidate.hasCalendarPrecision = true
		return candidate
	}
	if res, ok := resolveLexicalRelative(m.Text, anchor); ok {
		candidate.value = valueFromRelativeResolution(res)
		return candidate
	}
	if !m.Time.IsZero() {
		candidate.value = calendarValue(m.Time, CalendarPrecisionDay)
	}
	return candidate
}

func candidateFromRelative(rel *RelativeMatch, res relativeResolution) temporalCandidate {
	return temporalCandidate{
		span:     temporalSpan{Text: rel.Text, Index: rel.Index},
		source:   MatchSourceRelative,
		value:    valueFromRelativeResolution(res),
		priority: candidatePriorityRelative,
	}
}

func candidateFromDuration(d *DurationMatch) temporalCandidate {
	return temporalCandidate{
		span:     temporalSpan{Text: d.Text, Index: d.Index},
		source:   MatchSourceDuration,
		value:    temporalValue{Kind: ExpressionKindDuration, Timex: d.Timex},
		priority: candidatePriorityDuration,
	}
}

func candidateFromSet(s *SetMatch, anchor time.Time) temporalCandidate {
	value := temporalValue{
		Kind:         ExpressionKindSet,
		Timex:        s.Timex,
		Precision:    s.Precision,
		HasPrecision: true,
		Relative:     true,
	}
	value.Start, value.End, value.HasRange = rangeForPrecision(anchor, s.Precision)
	value.Time = value.Start
	return temporalCandidate{
		span:     temporalSpan{Text: s.Text, Index: s.Index},
		source:   MatchSourceSet,
		value:    value,
		priority: candidatePrioritySet,
	}
}

func calendarValue(t time.Time, precision CalendarPrecision) temporalValue {
	start, end, hasRange := rangeForPrecision(t, precision)
	return temporalValue{
		Kind:         kindForPrecision(precision),
		Timex:        timexForPrecision(start, precision),
		Time:         t,
		Precision:    precision,
		HasPrecision: true,
		Start:        start,
		End:          end,
		HasRange:     hasRange,
	}
}

func valueFromRelativeResolution(res relativeResolution) temporalValue {
	value := temporalValue{
		Time:         res.Time,
		Precision:    res.Precision,
		HasPrecision: res.HasPrecision,
		Relative:     true,
		Start:        res.Start,
		End:          res.End,
		HasRange:     res.HasRange,
	}
	if res.HasPrecision {
		value.Kind = kindForPrecision(res.Precision)
		value.Timex = timexForPrecision(res.Start, res.Precision)
	}
	return value
}

func kindForPrecision(precision CalendarPrecision) ExpressionKind {
	if precision == CalendarPrecisionDay {
		return ExpressionKindDate
	}
	return ExpressionKindDateRange
}

func rangeForPrecision(t time.Time, precision CalendarPrecision) (time.Time, time.Time, bool) {
	if t.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	switch precision {
	case CalendarPrecisionDay:
		start := startOfDay(t)
		return start, start.AddDate(0, 0, 1), true
	case CalendarPrecisionWeek:
		start := startOfWeek(t)
		return start, start.AddDate(0, 0, 7), true
	case CalendarPrecisionWeekend:
		start := startOfDay(t)
		return start, start.AddDate(0, 0, 2), true
	case CalendarPrecisionMonth:
		y, m, _ := t.Date()
		start := time.Date(y, m, 1, 0, 0, 0, 0, t.Location())
		return start, start.AddDate(0, 1, 0), true
	case CalendarPrecisionYear:
		y, _, _ := t.Date()
		start := time.Date(y, time.January, 1, 0, 0, 0, 0, t.Location())
		return start, start.AddDate(1, 0, 0), true
	default:
		return time.Time{}, time.Time{}, false
	}
}

func timexForPrecision(t time.Time, precision CalendarPrecision) string {
	if t.IsZero() {
		return ""
	}
	switch precision {
	case CalendarPrecisionDay:
		return t.Format("2006-01-02")
	case CalendarPrecisionWeek:
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	case CalendarPrecisionWeekend:
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d-WE", y, w)
	case CalendarPrecisionMonth:
		return t.Format("2006-01")
	case CalendarPrecisionYear:
		return fmt.Sprintf("%04d", t.Year())
	default:
		return ""
	}
}
