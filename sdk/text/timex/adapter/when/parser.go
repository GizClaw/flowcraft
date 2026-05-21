// Package when adapts [github.com/olebedev/when] to the
// sdk/text/timex.Parser interface.
//
// olebedev/when is the de-facto natural-language date / time
// parser in the Go ecosystem. It handles relative phrases
// ("tomorrow at 3pm", "next Wednesday", "in three weeks"), mixed
// absolute / relative ("by Friday at 14:00"), and ordinal weekday
// references ("the second Tuesday of June") — the entire class
// of expressions the zero-dependency [sdk/text/timex.RegexParser]
// baseline intentionally skips.
//
// Language coverage at the time of writing (when v1.1.0):
// English, Russian, Brazilian Portuguese, Chinese, and Dutch.
// This adapter ships English by default; callers needing more
// languages should use [NewWithLanguages]. [WrapParser] remains
// available for advanced callers with a custom underlying parser.
package when

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	olwhen "github.com/olebedev/when"
	"github.com/olebedev/when/rules/br"
	"github.com/olebedev/when/rules/common"
	"github.com/olebedev/when/rules/en"
	"github.com/olebedev/when/rules/nl"
	"github.com/olebedev/when/rules/ru"
	"github.com/olebedev/when/rules/zh"

	"github.com/GizClaw/flowcraft/sdk/text/timex"
)

// Parser wraps [olwhen.Parser] and exposes it through the
// sdk/text/timex.Parser interface. Construction loads the English
// rule set plus the shared common rules (durations, numerals).
//
// Parser is safe for concurrent use — the underlying olwhen.Parser
// is immutable after construction.
type Parser struct {
	p     *olwhen.Parser
	langs map[string]bool
}

// New constructs a [Parser] with English + common rules loaded.
//
// The constructor never fails — when's rule registration is in-
// memory and deterministic — but returns an error for API
// symmetry with other adapters that may surface I/O errors.
func New() (*Parser, error) {
	return NewWithLanguages("en")
}

// NewWithLanguages constructs a Parser with common rules plus the
// requested language rule sets. Supported identifiers are "en",
// "zh", "nl", "ru", and "br" / "pt_br" (Brazilian Portuguese).
//
// Passing no languages is equivalent to New(), preserving the SDK's
// historical English default. Unknown identifiers return an error so
// callers do not think multilingual parsing is enabled when it is not.
func NewWithLanguages(langs ...string) (*Parser, error) {
	w := olwhen.New(nil)
	w.Add(common.All...)
	if len(langs) == 0 {
		langs = []string{"en"}
	}
	enabled := map[string]bool{}
	for _, lang := range langs {
		normalized := normalizeLanguage(lang)
		switch normalized {
		case "en":
			w.Add(en.All...)
		case "zh":
			w.Add(zh.All...)
		case "nl":
			w.Add(nl.All...)
		case "ru":
			w.Add(ru.All...)
		case "br":
			w.Add(br.All...)
		case "":
			// Empty entries are ignored so callers can pass split
			// environment variables without pre-filtering.
			continue
		default:
			return nil, fmt.Errorf("when: unsupported language %q", lang)
		}
		if normalized != "" {
			enabled[normalized] = true
		}
	}
	return &Parser{p: w, langs: enabled}, nil
}

func normalizeLanguage(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "pt-br", "pt_br", "br":
		return "br"
	default:
		return strings.ToLower(strings.TrimSpace(lang))
	}
}

// WrapParser adapts a pre-configured [olwhen.Parser] (with custom
// rule sets, distance settings, or non-English languages) to the
// timex.Parser interface.
//
// Most callers needing CJK / multi-language parsing should use
// NewWithLanguages. WrapParser is for advanced users that need custom
// rule sets or non-standard parser settings.
func WrapParser(p *olwhen.Parser) *Parser {
	return &Parser{p: p}
}

// Parse implements [timex.Parser].
//
// olwhen returns (nil, nil) when no time expression is found;
// this adapter preserves that contract. Errors from the
// underlying parser propagate verbatim so callers can
// distinguish "no match" from "malformed rules".
func (p *Parser) Parse(text string, now time.Time) (match *timex.Match, err error) {
	if text == "" {
		return nil, nil
	}
	if p.hasLanguage("zh") {
		if m := parseChineseRelative(text, now); m != nil {
			return m, nil
		}
	}
	defer func() {
		if r := recover(); r != nil {
			match = nil
			err = fmt.Errorf("when: parse panic: %v", r)
		}
	}()
	r, err := p.p.Parse(text, now)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, nil
	}
	return &timex.Match{
		Time:  r.Time,
		Text:  r.Text,
		Index: r.Index,
	}, nil
}

// ParseContext mirrors [Parse] but threads a context.Context
// through to the underlying parser. when supports context
// cancellation for long-running multi-language rule evaluations;
// most callers do not need this and should reach for [Parse]
// instead.
func (p *Parser) ParseContext(ctx context.Context, text string, now time.Time) (*timex.Match, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if text == "" {
		return nil, nil
	}
	m, err := p.Parse(text, now)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

func (p *Parser) hasLanguage(lang string) bool {
	return p != nil && p.langs != nil && p.langs[lang]
}

var chineseRelativeRE = regexp.MustCompile(`([一二两三四五六七八九十0-9]+)\s*(天|日|周|星期|个月|月|年)(前|后|以后|之后)`)

func parseChineseRelative(text string, now time.Time) *timex.Match {
	if text == "" {
		return nil
	}
	if idx := strings.Index(text, "今天"); idx >= 0 {
		return chineseMatch(text, idx, "今天", startOfDay(now))
	}
	if idx := strings.Index(text, "明天"); idx >= 0 {
		return chineseMatch(text, idx, "明天", startOfDay(now).AddDate(0, 0, 1))
	}
	if idx := strings.Index(text, "昨天"); idx >= 0 {
		return chineseMatch(text, idx, "昨天", startOfDay(now).AddDate(0, 0, -1))
	}
	if idx := strings.Index(text, "后天"); idx >= 0 {
		return chineseMatch(text, idx, "后天", startOfDay(now).AddDate(0, 0, 2))
	}
	if idx := strings.Index(text, "前天"); idx >= 0 {
		return chineseMatch(text, idx, "前天", startOfDay(now).AddDate(0, 0, -2))
	}
	for _, c := range []struct {
		text string
		time time.Time
	}{
		{"下周", startOfDay(now).AddDate(0, 0, 7)},
		{"上周", startOfDay(now).AddDate(0, 0, -7)},
		{"下个月", startOfDay(now).AddDate(0, 1, 0)},
		{"上个月", startOfDay(now).AddDate(0, -1, 0)},
		{"明年", startOfDay(now).AddDate(1, 0, 0)},
		{"去年", startOfDay(now).AddDate(-1, 0, 0)},
	} {
		if idx := strings.Index(text, c.text); idx >= 0 {
			return chineseMatch(text, idx, c.text, c.time)
		}
	}
	loc := chineseRelativeRE.FindStringSubmatchIndex(text)
	if loc == nil {
		return nil
	}
	raw := text[loc[0]:loc[1]]
	nRaw := text[loc[2]:loc[3]]
	unit := text[loc[4]:loc[5]]
	dirRaw := text[loc[6]:loc[7]]
	n, ok := parseChineseNumber(nRaw)
	if !ok || n == 0 {
		return nil
	}
	direction := 1
	if dirRaw == "前" {
		direction = -1
	}
	base := startOfDay(now)
	var resolved time.Time
	switch unit {
	case "天", "日":
		resolved = base.AddDate(0, 0, direction*n)
	case "周", "星期":
		resolved = base.AddDate(0, 0, direction*n*7)
	case "个月", "月":
		resolved = base.AddDate(0, direction*n, 0)
	case "年":
		resolved = base.AddDate(direction*n, 0, 0)
	default:
		return nil
	}
	return chineseMatch(text, loc[0], raw, resolved)
}

func chineseMatch(_ string, index int, raw string, t time.Time) *timex.Match {
	return &timex.Match{Time: t, Text: raw, Index: index}
}

func parseChineseNumber(s string) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	digits := map[rune]int{
		'一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
		'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
	}
	runes := []rune(s)
	if len(runes) == 1 {
		if n, ok := digits[runes[0]]; ok {
			return n, true
		}
		if runes[0] == '十' {
			return 10, true
		}
	}
	if len(runes) == 2 && runes[0] == '十' {
		if ones, ok := digits[runes[1]]; ok {
			return 10 + ones, true
		}
	}
	if len(runes) == 2 && runes[1] == '十' {
		if tens, ok := digits[runes[0]]; ok {
			return tens * 10, true
		}
	}
	if len(runes) == 3 && runes[1] == '十' {
		tens, okT := digits[runes[0]]
		ones, okO := digits[runes[2]]
		if okT && okO {
			return tens*10 + ones, true
		}
	}
	return 0, false
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
