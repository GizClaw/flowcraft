package timex_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/text/timex"
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
	if text == "see you next Tuesday" {
		return &timex.Match{
			Time:  time.Date(2026, time.May, 26, 0, 0, 0, 0, time.UTC),
			Text:  "next Tuesday",
			Index: 8,
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
	if expr.Kind != timex.ExpressionKindDate || expr.Timex != "2023-10-13" {
		t.Fatalf("kind/timex = %s/%q, want date/2023-10-13", expr.Kind, expr.Timex)
	}
	assertRange(t, expr, time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC), time.Date(2023, time.October, 14, 0, 0, 0, 0, time.UTC))
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
	p := &recordingParser{}
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
	if expr.Kind != timex.ExpressionKindDate || expr.Timex != "2026-05-26" || !expr.HasRange {
		t.Fatalf("unexpected normalized natural expression: %+v", expr)
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
	if expr.Precision != timex.CalendarPrecisionWeek || !expr.HasPrecision {
		t.Fatalf("precision = %v has=%v, want week", expr.Precision, expr.HasPrecision)
	}
	if expr.Kind != timex.ExpressionKindDateRange || expr.Timex != "2026-W22" {
		t.Fatalf("kind/timex = %s/%q, want daterange/2026-W22", expr.Kind, expr.Timex)
	}
	assertRange(t, expr, time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
}

func TestExtractResolvesCountedWeekendAgo(t *testing.T) {
	anchor := time.Date(2023, 7, 17, 12, 0, 0, 0, time.UTC)
	expr, err := timex.Extract("Melanie went camping two weekends ago.", anchor)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if expr == nil {
		t.Fatal("expected lexical relative expression")
	}
	if expr.Text != "two weekends ago" {
		t.Fatalf("text = %q, want two weekends ago", expr.Text)
	}
	want := time.Date(2023, 7, 8, 0, 0, 0, 0, time.UTC)
	if !expr.Time.Equal(want) {
		t.Fatalf("time = %v, want %v", expr.Time, want)
	}
	if expr.Precision != timex.CalendarPrecisionWeekend || !expr.HasPrecision {
		t.Fatalf("precision = %v has=%v, want weekend", expr.Precision, expr.HasPrecision)
	}
	if expr.Kind != timex.ExpressionKindDateRange || expr.Timex != "2023-W27-WE" {
		t.Fatalf("kind/timex = %s/%q, want daterange/2023-W27-WE", expr.Kind, expr.Timex)
	}
	assertRange(t, expr, time.Date(2023, 7, 8, 0, 0, 0, 0, time.UTC), time.Date(2023, 7, 10, 0, 0, 0, 0, time.UTC))
}

func TestExtractResolvesCountedFuture(t *testing.T) {
	anchor := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	expr, err := timex.Extract("The trip starts in three weeks.", anchor)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if expr == nil {
		t.Fatal("expected lexical relative expression")
	}
	if expr.Text != "in three weeks" {
		t.Fatalf("text = %q, want in three weeks", expr.Text)
	}
	want := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	if !expr.Time.Equal(want) {
		t.Fatalf("time = %v, want %v", expr.Time, want)
	}
	if expr.Precision != timex.CalendarPrecisionWeek || !expr.HasPrecision {
		t.Fatalf("precision = %v has=%v, want week", expr.Precision, expr.HasPrecision)
	}
	assertRange(t, expr, time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
}

func TestExtractResolvesMultilingualCountedRelative(t *testing.T) {
	anchor := time.Date(2023, 5, 8, 13, 56, 0, 0, time.UTC)
	cases := []struct {
		text      string
		wantText  string
		wantTime  time.Time
		wantStart time.Time
		wantEnd   time.Time
		precision timex.CalendarPrecision
	}{
		{
			text:      "El viaje fue hace dos semanas.",
			wantText:  "hace dos semanas",
			wantTime:  time.Date(2023, 4, 24, 0, 0, 0, 0, time.UTC),
			wantStart: time.Date(2023, 4, 24, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC),
			precision: timex.CalendarPrecisionWeek,
		},
		{
			text:      "El viaje sera dentro de tres meses.",
			wantText:  "dentro de tres meses",
			wantTime:  time.Date(2023, 8, 8, 0, 0, 0, 0, time.UTC),
			wantStart: time.Date(2023, 8, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 9, 1, 0, 0, 0, 0, time.UTC),
			precision: timex.CalendarPrecisionMonth,
		},
		{
			text:      "Le voyage commence dans trois semaines.",
			wantText:  "dans trois semaines",
			wantTime:  time.Date(2023, 5, 29, 0, 0, 0, 0, time.UTC),
			wantStart: time.Date(2023, 5, 29, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 6, 5, 0, 0, 0, 0, time.UTC),
			precision: timex.CalendarPrecisionWeek,
		},
		{
			text:      "活动两个月后开始。",
			wantText:  "两个月后",
			wantTime:  time.Date(2023, 7, 8, 0, 0, 0, 0, time.UTC),
			wantStart: time.Date(2023, 7, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 8, 1, 0, 0, 0, 0, time.UTC),
			precision: timex.CalendarPrecisionMonth,
		},
		{
			text:      "活动三周前结束。",
			wantText:  "三周前",
			wantTime:  time.Date(2023, 4, 17, 0, 0, 0, 0, time.UTC),
			wantStart: time.Date(2023, 4, 17, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 4, 24, 0, 0, 0, 0, time.UTC),
			precision: timex.CalendarPrecisionWeek,
		},
		{
			text:      "Fuimos hace dos fines de semana.",
			wantText:  "hace dos fines de semana",
			wantTime:  time.Date(2023, 4, 29, 0, 0, 0, 0, time.UTC),
			wantStart: time.Date(2023, 4, 29, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC),
			precision: timex.CalendarPrecisionWeekend,
		},
	}
	for _, c := range cases {
		t.Run(c.wantText, func(t *testing.T) {
			expr, err := timex.Extract(c.text, anchor)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if expr == nil {
				t.Fatal("expected expression")
			}
			if expr.Text != c.wantText {
				t.Fatalf("text = %q, want %q", expr.Text, c.wantText)
			}
			if !expr.Time.Equal(c.wantTime) {
				t.Fatalf("time = %v, want %v", expr.Time, c.wantTime)
			}
			if expr.Precision != c.precision || !expr.HasPrecision {
				t.Fatalf("precision = %v has=%v, want %v", expr.Precision, expr.HasPrecision, c.precision)
			}
			assertRange(t, expr, c.wantStart, c.wantEnd)
		})
	}
}

func TestExtractResolvesDuration(t *testing.T) {
	cases := []struct {
		text     string
		wantText string
		want     string
	}{
		{"I stayed there for 4 years.", "for 4 years", "P4Y"},
		{"La visita duro durante dos semanas.", "durante dos semanas", "P2W"},
		{"项目为期三个月。", "为期三个月", "P3M"},
	}
	for _, c := range cases {
		t.Run(c.wantText, func(t *testing.T) {
			expr, err := timex.Extract(c.text, time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if expr == nil {
				t.Fatal("expected duration expression")
			}
			if expr.Source != timex.MatchSourceDuration || expr.Kind != timex.ExpressionKindDuration {
				t.Fatalf("unexpected expression: %+v", expr)
			}
			if expr.Text != c.wantText || expr.Timex != c.want {
				t.Fatalf("text/timex = %q/%q, want %q/%q", expr.Text, expr.Timex, c.wantText, c.want)
			}
		})
	}
}

func TestExtractResolvesSet(t *testing.T) {
	cases := []struct {
		text     string
		wantText string
		want     string
	}{
		{"We meet every Monday.", "every monday", "XXXX-WXX-1"},
		{"Tenemos revision cada semana.", "cada semana", "P1W"},
		{"每月复盘一次。", "每月", "P1M"},
	}
	for _, c := range cases {
		t.Run(c.wantText, func(t *testing.T) {
			expr, err := timex.Extract(c.text, time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if expr == nil {
				t.Fatal("expected set expression")
			}
			if expr.Source != timex.MatchSourceSet || expr.Kind != timex.ExpressionKindSet {
				t.Fatalf("unexpected expression: %+v", expr)
			}
			if expr.Text != c.wantText || expr.Timex != c.want {
				t.Fatalf("text/timex = %q/%q, want %q/%q", expr.Text, expr.Timex, c.wantText, c.want)
			}
		})
	}
}

func TestExtractResolvesExtendedCalendarRanges(t *testing.T) {
	anchor := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		text      string
		wantText  string
		wantStart time.Time
		wantEnd   time.Time
	}{
		{
			text:      "The project launched in Q2 2023.",
			wantText:  "Q2 2023",
			wantStart: time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 7, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			text:      "The trip happened in spring 2022.",
			wantText:  "spring 2022",
			wantStart: time.Date(2022, 3, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			text:      "Alice visited last summer.",
			wantText:  "last summer",
			wantStart: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			text:      "The release was in early May.",
			wantText:  "early May",
			wantStart: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC),
		},
		{
			text:      "The migration happened mid-2021.",
			wantText:  "mid-2021",
			wantStart: time.Date(2021, 5, 2, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2021, 9, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			text:      "Between May and June 2023, Alice traveled often.",
			wantText:  "Between May and June 2023",
			wantStart: time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 7, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			text:      "Alice had meetings since last year.",
			wantText:  "since last year",
			wantStart: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
		},
		{
			text:      "Alice is unavailable until next month.",
			wantText:  "until next month",
			wantStart: time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			text:      "The reminder is two weeks before July 2022.",
			wantText:  "two weeks before July 2022",
			wantStart: time.Date(2022, 6, 13, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2022, 6, 20, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		t.Run(c.wantText, func(t *testing.T) {
			expr, err := timex.Extract(c.text, anchor)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if expr == nil {
				t.Fatal("expected expression")
			}
			if expr.Text != c.wantText {
				t.Fatalf("text = %q, want %q", expr.Text, c.wantText)
			}
			if expr.Kind != timex.ExpressionKindDateRange || !expr.HasRange {
				t.Fatalf("unexpected range expression: %+v", expr)
			}
			assertRange(t, expr, c.wantStart, c.wantEnd)
		})
	}
}

func TestExtractResolvesCompoundChineseNumbers(t *testing.T) {
	anchor := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	expr, err := timex.Extract("活动二十三天后开始。", anchor)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if expr == nil {
		t.Fatal("expected expression")
	}
	want := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	if !expr.Time.Equal(want) {
		t.Fatalf("time = %v, want %v", expr.Time, want)
	}
}

func TestExtractResolvesExtendedDurationAndSet(t *testing.T) {
	cases := []struct {
		text     string
		wantText string
		want     string
		kind     timex.ExpressionKind
	}{
		{"I lived there for the past three years.", "for the past three years", "P3Y", timex.ExpressionKindDuration},
		{"The outage lasted over two months.", "over two months", "P2M", timex.ExpressionKindDuration},
		{"We meet twice a week.", "twice a week", "P1W", timex.ExpressionKindSet},
		{"We sync every other Monday.", "every other monday", "P2W", timex.ExpressionKindSet},
	}
	for _, c := range cases {
		t.Run(c.wantText, func(t *testing.T) {
			expr, err := timex.Extract(c.text, time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if expr == nil {
				t.Fatal("expected expression")
			}
			if expr.Text != c.wantText || expr.Timex != c.want || expr.Kind != c.kind {
				t.Fatalf("text/timex/kind = %q/%q/%s, want %q/%q/%s", expr.Text, expr.Timex, expr.Kind, c.wantText, c.want, c.kind)
			}
		})
	}
}

func TestExtractChoosesEarliestCandidate(t *testing.T) {
	anchor := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		text       string
		wantText   string
		wantSource timex.MatchSource
	}{
		{
			text:       "In July 2022 I stayed for 4 years.",
			wantText:   "July 2022",
			wantSource: timex.MatchSourceCalendar,
		},
		{
			text:       "I stayed for 4 years in July 2022.",
			wantText:   "for 4 years",
			wantSource: timex.MatchSourceDuration,
		},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			expr, err := timex.Extract(c.text, anchor)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if expr == nil {
				t.Fatal("expected expression")
			}
			if expr.Text != c.wantText || expr.Source != c.wantSource {
				t.Fatalf("text/source = %q/%s, want %q/%s", expr.Text, expr.Source, c.wantText, c.wantSource)
			}
		})
	}
}

func TestExtractRelativeRanges(t *testing.T) {
	anchor := time.Date(2023, 5, 8, 13, 56, 0, 0, time.UTC)
	cases := []struct {
		text      string
		wantTime  time.Time
		wantStart time.Time
		wantEnd   time.Time
		precision timex.CalendarPrecision
	}{
		{
			text:      "Melanie painted a lake sunrise last year.",
			wantTime:  time.Date(2022, 5, 8, 0, 0, 0, 0, time.UTC),
			wantStart: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
			precision: timex.CalendarPrecisionYear,
		},
		{
			text:      "The show is next month.",
			wantTime:  time.Date(2023, 6, 8, 0, 0, 0, 0, time.UTC),
			wantStart: time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 7, 1, 0, 0, 0, 0, time.UTC),
			precision: timex.CalendarPrecisionMonth,
		},
		{
			text:      "I went last Friday.",
			wantTime:  time.Date(2023, 5, 5, 0, 0, 0, 0, time.UTC),
			wantStart: time.Date(2023, 5, 5, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 5, 6, 0, 0, 0, 0, time.UTC),
			precision: timex.CalendarPrecisionDay,
		},
		{
			text:      "We went camping last weekend.",
			wantTime:  time.Date(2023, 5, 6, 0, 0, 0, 0, time.UTC),
			wantStart: time.Date(2023, 5, 6, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2023, 5, 8, 0, 0, 0, 0, time.UTC),
			precision: timex.CalendarPrecisionWeekend,
		},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			expr, err := timex.Extract(c.text, anchor)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if expr == nil {
				t.Fatal("expected expression")
			}
			if !expr.Time.Equal(c.wantTime) {
				t.Fatalf("time = %v, want %v", expr.Time, c.wantTime)
			}
			if expr.Precision != c.precision || !expr.HasPrecision {
				t.Fatalf("precision = %v has=%v, want %v", expr.Precision, expr.HasPrecision, c.precision)
			}
			assertRange(t, expr, c.wantStart, c.wantEnd)
		})
	}
}

func assertRange(t *testing.T, expr *timex.Expression, wantStart, wantEnd time.Time) {
	t.Helper()
	if !expr.HasRange {
		t.Fatalf("expected range on %+v", expr)
	}
	if !expr.Start.Equal(wantStart) || !expr.End.Equal(wantEnd) {
		t.Fatalf("range = [%v, %v), want [%v, %v)", expr.Start, expr.End, wantStart, wantEnd)
	}
}
