package timex_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/text/timex"
	whenadp "github.com/GizClaw/flowcraft/memory/text/timex/adapter/when"
)

type recordingParser struct {
	seen bool
}

func (p *recordingParser) Parse(text string, _ time.Time) (*timex.Match, error) {
	p.seen = true
	if text == "on October 13, 2023" {
		return &timex.Match{
			Time:  time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC),
			Text:  "October 13, 2023",
			Index: 3,
		}, nil
	}
	return nil, nil
}

func TestExtractPrefersCalendarPrecision(t *testing.T) {
	expr, err := timex.Extract("on October 13, 2023", time.Time{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if expr == nil {
		t.Fatal("expected expression")
	}
	if expr.Source != timex.MatchSourceCalendar || !expr.HasCalendarPrecision || expr.Precision != timex.CalendarPrecisionDay {
		t.Fatalf("unexpected expression: %+v", expr)
	}
}

func TestExtractLetsParserHandleWordDatesBeforeCalendarFallback(t *testing.T) {
	p := &recordingParser{}
	expr, err := timex.Extract("on October 13, 2023", time.Time{}, p)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !p.seen {
		t.Fatal("expected parser to see word-date text before calendar fallback")
	}
	if expr == nil {
		t.Fatal("expected expression")
	}
	if expr.Source != timex.MatchSourceNatural || !expr.HasCalendarPrecision || expr.Precision != timex.CalendarPrecisionDay {
		t.Fatalf("unexpected expression: %+v", expr)
	}
}

func TestExtractUsesNaturalParser(t *testing.T) {
	p, err := whenadp.New()
	if err != nil {
		t.Fatalf("NewWithLanguages: %v", err)
	}
	anchor := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	expr, err := timex.Extract("see you next Tuesday", anchor, p)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if expr == nil {
		t.Fatal("expected expression")
	}
	if expr.Source != timex.MatchSourceNatural || !expr.Relative {
		t.Fatalf("unexpected expression: %+v", expr)
	}
}

func TestExtractFallsBackToLexicalRelative(t *testing.T) {
	expr, err := timex.Extract("I went last Friday", time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if expr == nil {
		t.Fatal("expected lexical relative expression")
	}
	if expr.Source != timex.MatchSourceRelative || !expr.Relative {
		t.Fatalf("unexpected expression: %+v", expr)
	}
}

func TestExtractResolvesCoarseLexicalRelative(t *testing.T) {
	anchor := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	expr, err := timex.Extract("next week", anchor)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if expr == nil {
		t.Fatal("expected lexical relative expression")
	}
	want := time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)
	if !expr.Time.Equal(want) {
		t.Fatalf("time = %v, want %v", expr.Time, want)
	}
}
