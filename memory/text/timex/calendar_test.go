package timex

import (
	"testing"
	"time"
)

func TestParseCalendar(t *testing.T) {
	cases := []struct {
		text      string
		wantTime  time.Time
		precision CalendarPrecision
	}{
		{"on October 13, 2023", time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC), CalendarPrecisionDay},
		{"on 13 October 2023", time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC), CalendarPrecisionDay},
		{"in July 2022", time.Date(2022, time.July, 1, 0, 0, 0, 0, time.UTC), CalendarPrecisionMonth},
		{"during 2019", time.Date(2019, time.January, 1, 0, 0, 0, 0, time.UTC), CalendarPrecisionYear},
		{"2024-05-07", time.Date(2024, time.May, 7, 0, 0, 0, 0, time.UTC), CalendarPrecisionDay},
		{"el 13 octubre 2023", time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC), CalendarPrecisionDay},
		{"le 13 octobre 2023", time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC), CalendarPrecisionDay},
		{"am 13 Oktober 2023", time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC), CalendarPrecisionDay},
		{"em 13 outubro 2023", time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC), CalendarPrecisionDay},
		{"op 13 oktober 2023", time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC), CalendarPrecisionDay},
		{"13 октября 2023", time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC), CalendarPrecisionDay},
	}
	for _, c := range cases {
		got := ParseCalendar(c.text)
		if got == nil {
			t.Fatalf("ParseCalendar(%q) = nil", c.text)
		}
		if !got.Time.Equal(c.wantTime) || got.Precision != c.precision {
			t.Fatalf("ParseCalendar(%q) = %+v, want time=%s precision=%v", c.text, got, c.wantTime, c.precision)
		}
	}
}

func TestParseCalendarNoMatch(t *testing.T) {
	for _, text := range []string{"last Friday", "account 202", "hello"} {
		if got := ParseCalendar(text); got != nil {
			t.Fatalf("ParseCalendar(%q) = %+v, want nil", text, got)
		}
	}
}
