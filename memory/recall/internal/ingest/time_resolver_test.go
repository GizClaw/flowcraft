package ingest

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestTimeResolver_FillsObservedAt(t *testing.T) {
	r := passthroughTimeResolver{}
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	out := r.Resolve(domain.TemporalFact{Kind: domain.KindNote}, now)
	if !out.ObservedAt.Equal(now) {
		t.Errorf("ObservedAt = %v, want %v", out.ObservedAt, now)
	}
}

func TestTimeResolver_RelativeFromMeta(t *testing.T) {
	r := passthroughTimeResolver{}
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		hint   string
		want   time.Time
		wantTo *time.Time
	}{
		{"now", now, nil},
		{"today", time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC))},
		{"tomorrow", time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC))},
		{"yesterday", time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC))},
		{"next week", time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))},
		{"last week", time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC))},
		{"next month", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))},
		{"last month", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))},
		{"next year", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC))},
		{"last year", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))},
		{"4 years ago.", time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))},
		{"two weekends ago", time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC))},
		{"six months ago", time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC))},
		{"in 3 weeks", time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC), timePtr(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))},
	}
	for _, c := range cases {
		t.Run(c.hint, func(t *testing.T) {
			f := domain.TemporalFact{
				Kind:     domain.KindPlan,
				Metadata: map[string]any{MetaValidFromHint: c.hint},
			}
			out := r.Resolve(f, now)
			if out.ValidFrom == nil {
				t.Fatalf("expected ValidFrom for %q", c.hint)
			}
			if !out.ValidFrom.Equal(c.want) {
				t.Errorf("ValidFrom = %v, want %v", *out.ValidFrom, c.want)
			}
			if c.wantTo != nil {
				if out.ValidTo == nil || !out.ValidTo.Equal(*c.wantTo) {
					t.Errorf("ValidTo = %v, want %v", out.ValidTo, *c.wantTo)
				}
			}
			if _, leftover := out.Metadata[MetaValidFromHint]; leftover {
				t.Errorf("hint must be consumed from metadata")
			}
		})
	}
}

func timePtr(t time.Time) *time.Time {
	return &t
}

func TestTimeResolver_AbsoluteHintsFromLLM(t *testing.T) {
	r := passthroughTimeResolver{}
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		hint string
		want time.Time
	}{
		{"rfc3339", "2024-05-07T09:00:00Z", time.Date(2024, 5, 7, 9, 0, 0, 0, time.UTC)},
		{"date_only_iso", "2024-05-07", time.Date(2024, 5, 7, 0, 0, 0, 0, time.UTC)},
		{"date_slashes", "2024/05/07", time.Date(2024, 5, 7, 0, 0, 0, 0, time.UTC)},
		{"datetime_no_zone", "2024-05-07 09:00:00", time.Date(2024, 5, 7, 9, 0, 0, 0, time.UTC)},
		{"long_month", "January 2, 2024", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
		{"short_month", "Jan 2, 2024", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
		{"day_first_long", "2 January 2024", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
		{"day_first_short", "2 Jan 2024", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := domain.TemporalFact{
				Kind:     domain.KindEvent,
				Metadata: map[string]any{MetaValidFromHint: c.hint},
			}
			out := r.Resolve(f, now)
			if out.ValidFrom == nil {
				t.Fatalf("expected ValidFrom for %q", c.hint)
			}
			if !out.ValidFrom.Equal(c.want) {
				t.Errorf("ValidFrom = %v, want %v", *out.ValidFrom, c.want)
			}
			if _, leftover := out.Metadata[MetaValidFromHint]; leftover {
				t.Errorf("absolute hint must be consumed from metadata")
			}
		})
	}
}

func TestTimeResolver_ParsedTimeMetadataWinsOverRawHint(t *testing.T) {
	r := passthroughTimeResolver{}
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	want := time.Date(2019, 6, 27, 10, 37, 0, 0, time.UTC)
	f := domain.TemporalFact{
		Kind: domain.KindEvent,
		Metadata: map[string]any{
			MetaValidFromHint:   "四年前",
			MetaValidFromAt:     want.Format(time.RFC3339Nano),
			MetaValidFromSource: ValidFromSourceContentRelative,
		},
	}
	out := r.Resolve(f, now)
	if out.ValidFrom == nil {
		t.Fatal("expected ValidFrom from parsed metadata")
	}
	if !out.ValidFrom.Equal(want) {
		t.Errorf("ValidFrom = %v, want %v", *out.ValidFrom, want)
	}
	if _, leftover := out.Metadata[MetaValidFromAt]; leftover {
		t.Errorf("parsed metadata must be consumed")
	}
	if _, leftover := out.Metadata[MetaValidFromHint]; leftover {
		t.Errorf("raw hint should be consumed with parsed metadata")
	}
	if source, _ := out.Metadata[MetaValidFromSource].(string); source != ValidFromSourceContentRelative {
		t.Errorf("valid_from_source = %q, want %q", source, ValidFromSourceContentRelative)
	}
	if text, _ := out.Metadata[MetaValidFromText].(string); text != "四年前" {
		t.Errorf("valid_from_text = %q, want 四年前", text)
	}
}

func TestTimeResolver_UnknownHintLeavesNil(t *testing.T) {
	r := passthroughTimeResolver{}
	now := time.Now()
	f := domain.TemporalFact{
		Kind:     domain.KindPlan,
		Metadata: map[string]any{MetaValidToHint: "two thursdays hence"},
	}
	out := r.Resolve(f, now)
	if out.ValidTo != nil {
		t.Errorf("unparseable hint must keep ValidTo nil, got %v", *out.ValidTo)
	}
	// Hint is preserved when not consumed so callers can debug.
	if _, ok := out.Metadata[MetaValidToHint]; !ok {
		t.Error("unparseable hint should remain in metadata for debugging")
	}
}

func TestTimeResolver_DoesNotUseDurationOrSetAsValidFrom(t *testing.T) {
	r := passthroughTimeResolver{}
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cases := []string{
		"for the past three years",
		"over two months",
		"twice a year",
		"every other Monday",
	}
	for _, hint := range cases {
		t.Run(hint, func(t *testing.T) {
			f := domain.TemporalFact{
				Kind:     domain.KindState,
				Metadata: map[string]any{MetaValidFromHint: hint},
			}
			out := r.Resolve(f, now)
			if out.ValidFrom != nil {
				t.Fatalf("duration/set hint %q must not become ValidFrom, got %v", hint, *out.ValidFrom)
			}
			if _, ok := out.Metadata[MetaValidFromHint]; !ok {
				t.Fatalf("non-date hint should remain debuggable in metadata: %v", out.Metadata)
			}
		})
	}
}

func TestTimeResolver_DoesNotClobberCanonicalTimes(t *testing.T) {
	r := passthroughTimeResolver{}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	preset := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	f := domain.TemporalFact{
		Kind:      domain.KindPlan,
		ValidFrom: &preset,
		Metadata:  map[string]any{MetaValidFromHint: "tomorrow"},
	}
	out := r.Resolve(f, now)
	if !out.ValidFrom.Equal(preset) {
		t.Errorf("explicit ValidFrom must win over hint, got %v", *out.ValidFrom)
	}
}
