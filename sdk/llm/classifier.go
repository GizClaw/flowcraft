package llm

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
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

// ClassifyProviderError wraps a provider error into an errdefs-classified
// error so that pod-level policies (retry, circuit breaking, fail-fast on
// auth) can decide on it via the standard errdefs.IsXxx checks. The wrap
// preserves the original error chain so callers that need finer-grained
// signals (LLM ErrorCategory, vendor-specific fields) can still
// errors.As against the inner type.
//
// Mapping (mirrors ErrorCategory but in errdefs vocabulary):
//
//	CategoryAuth            → Unauthorized
//	CategoryBilling         → Forbidden     (long-term unusable, not a 429)
//	CategoryRateLimit       → RateLimit
//	CategoryContextOverflow → Validation    (input too large; client must shrink)
//	CategoryPermanent       → Validation    (client error; bad request)
//	CategoryTransient       → NotAvailable  (5xx / network flake; safe to retry)
//
// Returns nil when err is nil; returns the original err untouched when it
// already carries any errdefs classification (so callers can pre-classify
// known cases like ctx.Err()).
func ClassifyProviderError(provider string, err error) error {
	if err == nil {
		return nil
	}
	if errdefs.HasClassification(err) {
		return err
	}
	wrapped := fmt.Errorf("%s: %w", provider, err)
	switch ClassifyError(err) {
	case CategoryAuth:
		return errdefs.Unauthorized(wrapped)
	case CategoryBilling:
		return errdefs.Forbidden(wrapped)
	case CategoryRateLimit:
		return errdefs.RateLimit(wrapped)
	case CategoryContextOverflow, CategoryPermanent:
		return errdefs.Validation(wrapped)
	default: // CategoryTransient
		return errdefs.NotAvailable(wrapped)
	}
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

// ClassifyHTTPStatus wraps a raw HTTP status code into an
// errdefs-classified error. It is the entry point for providers that
// drive HTTP themselves (e.g. ollama, custom REST adapters) and need to
// turn a non-2xx response into the same errdefs vocabulary that
// ClassifyProviderError produces from SDK error types.
//
// body is included verbatim in the error message to preserve provider
// diagnostics; callers may pass an empty string if no body is
// available. Codes below 400 always map to Internal — we expect
// callers to gate this helper behind `code < 200 || code >= 300`.
func ClassifyHTTPStatus(provider string, code int, body string) error {
	msg := fmt.Sprintf("%s: http %d", provider, code)
	if body != "" {
		msg = fmt.Sprintf("%s: %s", msg, body)
	}
	wrapped := errors.New(msg)
	if cat, ok := categoryFromHTTPCode(code); ok {
		switch cat {
		case CategoryAuth:
			return errdefs.Unauthorized(wrapped)
		case CategoryBilling:
			return errdefs.Forbidden(wrapped)
		case CategoryRateLimit:
			return errdefs.RateLimit(wrapped)
		case CategoryPermanent:
			return errdefs.Validation(wrapped)
		}
	}
	if code >= 500 {
		return errdefs.NotAvailable(wrapped)
	}
	return errdefs.Internal(wrapped)
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
