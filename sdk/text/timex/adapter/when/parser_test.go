package when_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/text/timex"
	"github.com/GizClaw/flowcraft/sdk/text/timex/adapter/when"
)

func newParser(t *testing.T) *when.Parser {
	t.Helper()
	p, err := when.New()
	if err != nil {
		t.Fatalf("when.New: %v", err)
	}
	return p
}

// TestParser_SatisfiesTimexInterface pins the interface contract.
// If the underlying olwhen.Parser shape drifts and we accidentally
// break the timex.Parser implementation, callers holding
// timex.Parser references would silently fail at runtime — the
// type assertion catches it at compile time.
func TestParser_SatisfiesTimexInterface(t *testing.T) {
	var _ timex.Parser = newParser(t)
}

func TestParse_AbsoluteDate(t *testing.T) {
	p := newParser(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	m, err := p.Parse("the deadline is May 30, 2026", now)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m == nil {
		t.Fatal("expected match for explicit date")
	}
	if m.Time.Year() != 2026 || m.Time.Month() != time.May || m.Time.Day() != 30 {
		t.Errorf("Time = %v, want 2026-05-30", m.Time)
	}
}

func TestParse_RelativeWeekday(t *testing.T) {
	p := newParser(t)
	// Wednesday 2026-05-20; "next Tuesday" should resolve to
	// 2026-05-26 (the Tuesday of the FOLLOWING week, not
	// tomorrow's Tuesday — relative weekdays look forward by at
	// least 7 days when the same week is ambiguous).
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	m, err := p.Parse("see you next Tuesday", now)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m == nil {
		t.Fatal("expected match for 'next Tuesday'")
	}
	if m.Time.Weekday() != time.Tuesday {
		t.Errorf("Weekday = %v, want Tuesday", m.Time.Weekday())
	}
	if !m.Time.After(now) {
		t.Errorf("Time = %v should be strictly after %v", m.Time, now)
	}
}

func TestParse_NoMatch(t *testing.T) {
	p := newParser(t)
	m, err := p.Parse("just some normal sentence with no time", time.Now())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil match, got %+v", m)
	}
}

func TestParse_Empty(t *testing.T) {
	p := newParser(t)
	m, err := p.Parse("", time.Now())
	if err != nil || m != nil {
		t.Errorf("empty input must be a no-op, got match=%v err=%v", m, err)
	}
}
