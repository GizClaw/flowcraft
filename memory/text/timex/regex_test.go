package timex_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/text/timex"
)

func TestRegexParser_ISO8601Date(t *testing.T) {
	m, err := timex.RegexParser{}.Parse("delivered on 2026-05-20", time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m == nil {
		t.Fatal("expected match")
	}
	want := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if !m.Time.Equal(want) {
		t.Errorf("Time = %v, want %v", m.Time, want)
	}
	if m.Text != "2026-05-20" {
		t.Errorf("Text = %q, want 2026-05-20", m.Text)
	}
	if m.Index != 13 {
		t.Errorf("Index = %d, want 13", m.Index)
	}
}

func TestRegexParser_ISO8601Timestamp(t *testing.T) {
	m, err := timex.RegexParser{}.Parse("event at 2026-05-20T14:30:00Z details", time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m == nil {
		t.Fatal("expected match")
	}
	want := time.Date(2026, 5, 20, 14, 30, 0, 0, time.UTC)
	if !m.Time.Equal(want) {
		t.Errorf("Time = %v, want %v", m.Time, want)
	}
}

func TestRegexParser_USSlash(t *testing.T) {
	m, err := timex.RegexParser{}.Parse("5/20/2026 meeting", time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m == nil {
		t.Fatal("expected match")
	}
	want := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if !m.Time.Equal(want) {
		t.Errorf("Time = %v, want %v", m.Time, want)
	}
}

func TestRegexParser_NoMatch(t *testing.T) {
	m, err := timex.RegexParser{}.Parse("yesterday at noon", time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil match for NL-only phrase, got %+v", m)
	}
}

// TestRegexParser_TimestampWinsOverDate guards the precision-order
// rule: when both an ISO date and an ISO timestamp could match
// inside the same input, the timestamp wins because it carries
// strictly more information.
func TestRegexParser_TimestampWinsOverDate(t *testing.T) {
	m, err := timex.RegexParser{}.Parse("2026-05-20 and 2026-05-21T09:00:00Z", time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m == nil {
		t.Fatal("expected match")
	}
	if m.Text != "2026-05-21T09:00:00Z" {
		t.Errorf("timestamp must win, got %q", m.Text)
	}
}

func TestRegexParser_Empty(t *testing.T) {
	m, err := timex.RegexParser{}.Parse("", time.Now())
	if err != nil || m != nil {
		t.Errorf("empty input must be a no-op, got match=%v err=%v", m, err)
	}
}

// _ guard: the package-level RegexParser must satisfy the Parser
// interface so swapping with adapter implementations is type-safe.
var _ timex.Parser = timex.RegexParser{}
