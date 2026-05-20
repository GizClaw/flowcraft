package timex

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// RegexParser is the zero-dependency baseline [Parser]. It
// recognises three absolute date shapes that cover the bulk of
// machine-generated text:
//
//   - ISO 8601 date: 2026-05-20
//   - ISO 8601 timestamp: 2026-05-20T14:30:00Z (Z or ±HH:MM)
//   - US slash date: 5/20/2026 or 05/20/2026
//
// Relative phrases ("yesterday", "next Tuesday") are NOT handled
// — adapter sub-packages (timex/adapter/when) cover those.
// Keeping the baseline narrow makes its behaviour easy to reason
// about and impossible to regress.
//
// RegexParser is safe for concurrent use; it holds no state
// beyond the compiled patterns, which are package-level globals
// so construction is O(1).
type RegexParser struct{}

// Parse implements [Parser].
//
// On a successful match RegexParser returns the FIRST recognised
// date in text — left-to-right scan. Subsequent dates are
// ignored; callers wanting every match should pre-tokenise the
// input or use a richer NL parser.
func (RegexParser) Parse(text string, _ time.Time) (*Match, error) {
	if text == "" {
		return nil, nil
	}
	// Try patterns in precision order: timestamp > date > slash
	// so the most-specific match wins when shapes overlap inside
	// the same input.
	if m, err := matchISO8601Timestamp(text); err != nil {
		return nil, err
	} else if m != nil {
		return m, nil
	}
	if m, err := matchISO8601Date(text); err != nil {
		return nil, err
	} else if m != nil {
		return m, nil
	}
	if m, err := matchUSSlashDate(text); err != nil {
		return nil, err
	} else if m != nil {
		return m, nil
	}
	return nil, nil
}

var (
	iso8601TimestampRE = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))\b`)
	iso8601DateRE      = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)
	usSlashDateRE      = regexp.MustCompile(`\b(\d{1,2})/(\d{1,2})/(\d{4})\b`)
)

func matchISO8601Timestamp(text string) (*Match, error) {
	loc := iso8601TimestampRE.FindStringSubmatchIndex(text)
	if loc == nil {
		return nil, nil
	}
	raw := text[loc[2]:loc[3]]
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, fmt.Errorf("timex: parse RFC3339 %q: %w", raw, err)
	}
	return &Match{Time: t.UTC(), Text: raw, Index: loc[2]}, nil
}

func matchISO8601Date(text string) (*Match, error) {
	loc := iso8601DateRE.FindStringSubmatchIndex(text)
	if loc == nil {
		return nil, nil
	}
	raw := text[loc[2]:loc[3]]
	t, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return nil, fmt.Errorf("timex: parse ISO date %q: %w", raw, err)
	}
	return &Match{Time: t, Text: raw, Index: loc[2]}, nil
}

func matchUSSlashDate(text string) (*Match, error) {
	loc := usSlashDateRE.FindStringSubmatchIndex(text)
	if loc == nil {
		return nil, nil
	}
	raw := text[loc[0]:loc[1]]
	month, _ := strconv.Atoi(text[loc[2]:loc[3]])
	day, _ := strconv.Atoi(text[loc[4]:loc[5]])
	year, _ := strconv.Atoi(text[loc[6]:loc[7]])
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return nil, fmt.Errorf("timex: invalid US slash date %q", raw)
	}
	return &Match{
		Time:  time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC),
		Text:  raw,
		Index: loc[0],
	}, nil
}
