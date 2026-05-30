package timex

import "time"

const (
	candidatePriorityNumericCalendar = 10
	candidatePriorityNatural         = 20
	candidatePriorityDuration        = 30
	candidatePrioritySet             = 30
	candidatePriorityCalendarRange   = 35
	candidatePriorityCalendarWords   = 40
	candidatePriorityRelative        = 50
)

type temporalSpan struct {
	Text  string
	Index int
}

type temporalValue struct {
	Kind         ExpressionKind
	Timex        string
	Time         time.Time
	Precision    CalendarPrecision
	HasPrecision bool
	Relative     bool
	Start        time.Time
	End          time.Time
	HasRange     bool
}

type temporalCandidate struct {
	span                 temporalSpan
	source               MatchSource
	value                temporalValue
	hasCalendarPrecision bool
	priority             int
}

type temporalRecognizer interface {
	Recognize(text string, anchor time.Time) ([]temporalCandidate, error)
}

func runRecognizers(text string, anchor time.Time, recognizers ...temporalRecognizer) ([]temporalCandidate, error) {
	var candidates []temporalCandidate
	var firstErr error
	for _, recognizer := range recognizers {
		if recognizer == nil {
			continue
		}
		found, err := recognizer.Recognize(text, anchor)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		candidates = append(candidates, found...)
	}
	return candidates, firstErr
}

func selectFirstCandidate(candidates []temporalCandidate) (temporalCandidate, bool) {
	if len(candidates) == 0 {
		return temporalCandidate{}, false
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.span.Index < best.span.Index ||
			(candidate.span.Index == best.span.Index && candidate.priority < best.priority) {
			best = candidate
		}
	}
	return best, true
}

type numericCalendarRecognizer struct{}

func (numericCalendarRecognizer) Recognize(text string, _ time.Time) ([]temporalCandidate, error) {
	cal := ParseNumericCalendar(text)
	if cal == nil {
		return nil, nil
	}
	return []temporalCandidate{candidateFromCalendar(cal, MatchSourceCalendar, candidatePriorityNumericCalendar)}, nil
}

type calendarWordsRecognizer struct{}

func (calendarWordsRecognizer) Recognize(text string, _ time.Time) ([]temporalCandidate, error) {
	cal := parseCalendarWords(text)
	if cal == nil {
		return nil, nil
	}
	return []temporalCandidate{candidateFromCalendar(cal, MatchSourceCalendar, candidatePriorityCalendarWords)}, nil
}

type calendarRangeRecognizer struct{}

func (calendarRangeRecognizer) Recognize(text string, anchor time.Time) ([]temporalCandidate, error) {
	match := findCalendarRange(text, anchor)
	if match == nil {
		return nil, nil
	}
	return []temporalCandidate{candidateFromCalendarRange(match)}, nil
}

type parserRecognizer struct {
	parsers []Parser
}

func (r parserRecognizer) Recognize(text string, anchor time.Time) ([]temporalCandidate, error) {
	var firstErr error
	for _, parser := range r.parsers {
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
		return []temporalCandidate{candidateFromNaturalMatch(m, anchor)}, nil
	}
	return nil, firstErr
}

type relativeRecognizer struct{}

func (relativeRecognizer) Recognize(text string, anchor time.Time) ([]temporalCandidate, error) {
	rel := FindRelativePhrase(text)
	if rel == nil {
		return nil, nil
	}
	res, ok := resolveLexicalRelative(rel.Text, anchor)
	if !ok {
		res = relativeResolution{}
	}
	return []temporalCandidate{candidateFromRelative(rel, res)}, nil
}

type durationRecognizer struct{}

func (durationRecognizer) Recognize(text string, _ time.Time) ([]temporalCandidate, error) {
	dur := FindDurationPhrase(text)
	if dur == nil {
		return nil, nil
	}
	return []temporalCandidate{candidateFromDuration(dur)}, nil
}

type setRecognizer struct{}

func (setRecognizer) Recognize(text string, anchor time.Time) ([]temporalCandidate, error) {
	set := FindSetPhrase(text)
	if set == nil {
		return nil, nil
	}
	return []temporalCandidate{candidateFromSet(set, anchor)}, nil
}
