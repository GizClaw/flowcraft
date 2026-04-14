package llm

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// ErrorCategory classifies LLM provider errors for fallback decisions.
type ErrorCategory int

const (
	CategoryTransient       ErrorCategory = iota // network flake, 5xx — retry/fallback
	CategoryRateLimit                            // 429 — cooldown current provider then fallback
	CategoryAuth                                 // 401/403 — this provider is unusable
	CategoryBilling                              // 402 — long-term unusable
	CategoryContextOverflow                      // context length exceeded — all providers will fail
	CategoryPermanent                            // 400/422 — client error, no fallback
)

func (c ErrorCategory) String() string {
	switch c {
	case CategoryRateLimit:
		return "rate_limit"
	case CategoryAuth:
		return "auth"
	case CategoryBilling:
		return "billing"
	case CategoryContextOverflow:
		return "context_overflow"
	case CategoryPermanent:
		return "permanent"
	default:
		return "transient"
	}
}

// ShouldFallback reports whether this category should try the next provider.
func (c ErrorCategory) ShouldFallback() bool {
	switch c {
	case CategoryPermanent, CategoryContextOverflow:
		return false
	default:
		return true
	}
}

// CooldownMultiplier returns a multiplier for the base cooldown duration.
func (c ErrorCategory) CooldownMultiplier() int {
	switch c {
	case CategoryAuth, CategoryBilling:
		return 10
	case CategoryRateLimit:
		return 3
	default:
		return 1
	}
}

var httpCodePattern = regexp.MustCompile(`\b(?:http|status)\s*(\d{3})\b`)

// keyword classifiers ordered by priority — checked before HTTP status codes
// so that "http 400: maximum context length exceeded" is classified as
// ContextOverflow rather than Permanent.
var keywordClassifiers = []struct {
	category ErrorCategory
	keywords []string
}{
	{CategoryContextOverflow, []string{
		"context length exceeded",
		"maximum context length",
		"too many tokens",
		"content too large",
		"request too large",
		"payload too large",
		"max_tokens",
	}},
	{CategoryAuth, []string{
		"invalid api key",
		"incorrect api key",
		"unauthorized",
		"authentication",
		"permission denied",
		"api key not found",
		"invalid_api_key",
		"invalid x-api-key",
	}},
	{CategoryBilling, []string{
		"insufficient credits",
		"insufficient credit",
		"billing",
		"payment required",
		"quota exceeded",
		"insufficient_quota",
		"no credit",
	}},
	{CategoryRateLimit, []string{
		"rate limit",
		"rate_limit",
		"too many requests",
		"overloaded",
		"resource_exhausted",
		"at capacity",
		"capacity exceeded",
		"server is busy",
		"currently overloaded",
	}},
}

// HTTPStatusCoder is implemented by errors that carry an HTTP status code.
// ClassifyError checks this interface first for the most reliable classification.
type HTTPStatusCoder interface {
	HTTPStatusCode() int
}

// ClassifyError determines the error category for fallback decisions.
// Match order: structured interface (HTTPStatusCoder) → keywords → HTTP status code regex.
func ClassifyError(err error) ErrorCategory {
	if err == nil {
		return CategoryTransient
	}

	var coded HTTPStatusCoder
	if errors.As(err, &coded) {
		if cat, ok := categoryFromHTTPCode(coded.HTTPStatusCode()); ok {
			return cat
		}
	}

	msg := collectErrorMessages(err)

	for _, c := range keywordClassifiers {
		for _, kw := range c.keywords {
			if strings.Contains(msg, kw) {
				return c.category
			}
		}
	}

	if m := httpCodePattern.FindStringSubmatch(msg); len(m) == 2 {
		if code, e := strconv.Atoi(m[1]); e == nil {
			if cat, ok := categoryFromHTTPCode(code); ok {
				return cat
			}
		}
	}

	return CategoryTransient
}

func categoryFromHTTPCode(code int) (ErrorCategory, bool) {
	switch code {
	case 401, 403:
		return CategoryAuth, true
	case 402:
		return CategoryBilling, true
	case 429:
		return CategoryRateLimit, true
	case 400, 404, 405, 422:
		return CategoryPermanent, true
	default:
		return CategoryTransient, false
	}
}

// collectErrorMessages walks the error chain (including multi-errors from
// errors.Join / multiple %w) and concatenates all messages.
func collectErrorMessages(err error) string {
	var b strings.Builder
	var walk func(error)
	walk = func(e error) {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strings.ToLower(e.Error()))
		switch x := e.(type) {
		case interface{ Unwrap() error }:
			if u := x.Unwrap(); u != nil {
				walk(u)
			}
		case interface{ Unwrap() []error }:
			for _, u := range x.Unwrap() {
				walk(u)
			}
		}
	}
	walk(err)
	return b.String()
}
