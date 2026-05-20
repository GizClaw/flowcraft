package ingest

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Metadata keys consulted by the default port.TimeResolver. Callers
// (typically the LLM extractor) write structured relative-time
// strings into these keys; the resolver reads-and-strips them so
// the canonical fact stays clean.
const (
	MetaValidFromHint = "valid_from_hint"
	MetaValidToHint   = "valid_to_hint"
)

// SupportedRelativeTimes lists the tokens the default
// TimeResolver understands. Useful for tests / extractor schemas.
var SupportedRelativeTimes = []string{
	"now",
	"today",
	"tomorrow",
	"yesterday",
	"next week",
	"last week",
	"next month",
	"last month",
	"next year",
	"last year",
}

type passthroughTimeResolver struct{}

var _ port.TimeResolver = passthroughTimeResolver{}

// Resolve implements port.TimeResolver.
func (passthroughTimeResolver) Resolve(f domain.TemporalFact, now time.Time) domain.TemporalFact {
	if f.ObservedAt.IsZero() {
		f.ObservedAt = now
	}
	if f.ValidFrom == nil {
		if t, ok := parseRelativeFromMeta(f.Metadata, MetaValidFromHint, now); ok {
			tt := t
			f.ValidFrom = &tt
			delete(f.Metadata, MetaValidFromHint)
		}
	}
	if f.ValidTo == nil {
		if t, ok := parseRelativeFromMeta(f.Metadata, MetaValidToHint, now); ok {
			tt := t
			f.ValidTo = &tt
			delete(f.Metadata, MetaValidToHint)
		}
	}
	return f
}

// parseRelativeFromMeta extracts and parses a relative-time hint
// from Metadata. Non-string or unrecognised values return ok=false
// so the canonical field stays nil. Recognised hints have their
// metadata entry consumed by the caller.
func parseRelativeFromMeta(meta map[string]any, key string, now time.Time) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	raw, ok := meta[key]
	if !ok {
		return time.Time{}, false
	}
	s, ok := raw.(string)
	if !ok {
		return time.Time{}, false
	}
	return parseRelativeEnglish(s, now)
}

// parseRelativeEnglish handles the small English relative-time
// subset spelled out in SupportedRelativeTimes, plus a handful of
// absolute formats that the LLM extractor is asked to emit when a
// snippet states a calendar date or datetime. Inputs are
// trimmed + lower-cased for the relative table; absolute formats
// are parsed against the original (case-preserving) string so
// "January" / "Jan" survive. Unrecognised strings return ok=false.
func parseRelativeEnglish(in string, now time.Time) (time.Time, bool) {
	raw := strings.TrimSpace(in)
	if raw == "" {
		return time.Time{}, false
	}
	if t, ok := parseAbsoluteTime(raw); ok {
		return t, true
	}
	token := strings.ToLower(strings.Join(strings.Fields(raw), " "))
	switch token {
	case "now":
		return now, true
	case "today":
		return startOfDay(now), true
	case "tomorrow":
		return startOfDay(now).AddDate(0, 0, 1), true
	case "yesterday":
		return startOfDay(now).AddDate(0, 0, -1), true
	case "next week":
		return startOfDay(now).AddDate(0, 0, 7), true
	case "last week":
		return startOfDay(now).AddDate(0, 0, -7), true
	case "next month":
		return startOfDay(now).AddDate(0, 1, 0), true
	case "last month":
		return startOfDay(now).AddDate(0, -1, 0), true
	case "next year":
		return startOfDay(now).AddDate(1, 0, 0), true
	case "last year":
		return startOfDay(now).AddDate(-1, 0, 0), true
	}
	return time.Time{}, false
}

// absoluteTimeLayouts are the calendar shapes the resolver accepts
// alongside the relative token table. Order matters: more specific
// layouts come first so an RFC3339 timestamp is not mis-parsed as a
// bare date. The set is intentionally small — anything beyond this
// belongs to a locale-aware time library, not the canonical resolver.
var absoluteTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02",
	"January 2, 2006",
	"Jan 2, 2006",
	"2 January 2006",
	"2 Jan 2006",
}

// parseAbsoluteTime returns the first absolute-format match against
// the configured layouts. Returns ok=false when no layout parses.
func parseAbsoluteTime(raw string) (time.Time, bool) {
	for _, layout := range absoluteTimeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
