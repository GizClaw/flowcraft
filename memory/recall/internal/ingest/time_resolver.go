package ingest

import (
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/text/timex"
	whenadp "github.com/GizClaw/flowcraft/memory/text/timex/adapter/when"
)

// Metadata keys consulted by the default port.TimeResolver. Callers
// (typically the Structurizer or LLM extractor) write either raw time
// hints (*Hint) or already parsed absolute timestamps (*At) into
// these keys; the resolver reads-and-strips them so the canonical fact
// stays clean.
const (
	MetaValidFromHint = "valid_from_hint"
	MetaValidToHint   = "valid_to_hint"
	MetaValidFromAt   = "valid_from_at"
	MetaValidToAt     = "valid_to_at"

	MetaValidFromSource = "valid_from_source"
	MetaValidFromText   = "valid_from_text"
)

const (
	ValidFromSourceContentExplicit = "content_explicit"
	ValidFromSourceContentRelative = "content_relative"
	ValidFromSourceTimeFallback    = "source_time_fallback"
)

type passthroughTimeResolver struct{}

var _ port.TimeResolver = passthroughTimeResolver{}

// Resolve implements port.TimeResolver.
func (passthroughTimeResolver) Resolve(f domain.TemporalFact, now time.Time) domain.TemporalFact {
	if f.ObservedAt.IsZero() {
		f.ObservedAt = now
	}
	if f.ValidFrom == nil {
		if t, ok := parseAbsoluteFromMeta(f.Metadata, MetaValidFromAt); ok {
			tt := t
			f.ValidFrom = &tt
			preserveValidFromText(f.Metadata)
			delete(f.Metadata, MetaValidFromAt)
			delete(f.Metadata, MetaValidFromHint)
		} else if t, ok := parseRelativeFromMeta(f.Metadata, MetaValidFromHint, now); ok {
			tt := t
			f.ValidFrom = &tt
			preserveValidFromText(f.Metadata)
			delete(f.Metadata, MetaValidFromHint)
		}
	}
	if f.ValidTo == nil {
		if t, ok := parseAbsoluteFromMeta(f.Metadata, MetaValidToAt); ok {
			tt := t
			f.ValidTo = &tt
			delete(f.Metadata, MetaValidToAt)
			delete(f.Metadata, MetaValidToHint)
		} else if t, ok := parseRelativeFromMeta(f.Metadata, MetaValidToHint, now); ok {
			tt := t
			f.ValidTo = &tt
			delete(f.Metadata, MetaValidToHint)
		}
	}
	return f
}

func preserveValidFromText(meta map[string]any) {
	if len(meta) == 0 {
		return
	}
	if _, ok := meta[MetaValidFromText]; ok {
		return
	}
	if raw, ok := meta[MetaValidFromHint]; ok {
		meta[MetaValidFromText] = raw
	}
}

func parseAbsoluteFromMeta(meta map[string]any, key string) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	raw, ok := meta[key]
	if !ok {
		return time.Time{}, false
	}
	switch v := raw.(type) {
	case time.Time:
		if v.IsZero() {
			return time.Time{}, false
		}
		return v, true
	case string:
		if t, ok := parseTimeHint(strings.TrimSpace(v), time.Time{}, false); ok {
			return t, true
		}
	}
	return time.Time{}, false
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
	return parseTimeHint(s, now, true)
}

func parseTimeHint(in string, now time.Time, allowRelative bool) (time.Time, bool) {
	raw := strings.Trim(strings.TrimSpace(in), `"'.,;:!?()[]{} `)
	if raw == "" {
		return time.Time{}, false
	}
	if cal := timex.ParseCalendar(raw); cal != nil {
		return cal.Time.UTC(), true
	}
	parsers := timeHintParsers()
	if !allowRelative {
		parsers = nil
	}
	expr, err := timex.Extract(raw, now, parsers...)
	if err != nil || expr == nil || expr.Time.IsZero() {
		return time.Time{}, false
	}
	if expr.Relative {
		if !allowRelative {
			return time.Time{}, false
		}
		if strings.EqualFold(strings.TrimSpace(expr.Text), "now") {
			return expr.Time, true
		}
		return startOfDay(expr.Time), true
	}
	return expr.Time.UTC(), true
}

func timeHintParsers() []timex.Parser {
	parsers, err := timeHintParserSet()
	if err != nil {
		return nil
	}
	return parsers
}

var timeHintParserSet = sync.OnceValues(func() ([]timex.Parser, error) {
	var out []timex.Parser
	if p, err := whenadp.New(); err == nil {
		out = append(out, p)
	} else {
		return nil, err
	}
	if p, err := whenadp.NewWithLanguages("zh"); err == nil {
		out = append(out, p)
	} else {
		return nil, err
	}
	return out, nil
})

func startOfDay(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
