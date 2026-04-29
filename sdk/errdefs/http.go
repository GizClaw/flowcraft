package errdefs

// Provider error normalisation.
//
// errors.go defines the *forward* mapping: an errdefs-classified error can
// answer HTTPStatus(err) → an HTTP status code suitable for a wire response.
// This file defines the *reverse* mapping for the SDK boundary: take an
// arbitrary error coming back from an external provider (HTTP SDK error,
// raw HTTP status, plain message string) and turn it into an errdefs
// classification that the rest of the system — pod controller, fallback
// logic, retry middleware — can act on uniformly via the IsXxx checks.
//
// The reverse mapping is shared by every SDK boundary that talks to an
// external provider over HTTP/SaaS: chat completion, embeddings, rerank,
// vector search, TTS/STT, knowledge ingestion, etc. It deliberately lives
// in errdefs (not in sdk/llm) so peer capabilities are not forced through
// the LLM package just to reuse the classification.
//
// Two entry points:
//
//   - ClassifyProviderError(provider, err) — when a provider SDK has done
//     the HTTP exchange already and surfaced its own error type. The
//     classifier inspects HTTPStatusCoder, then known phrase keywords,
//     then an embedded "http NNN" status pattern.
//
//   - ClassifyHTTPStatus(provider, code, body) — when the caller is
//     driving HTTP itself (raw net/http, custom REST adapter) and only
//     has a status code + body. Caller should gate on a non-2xx response
//     and let this helper turn the code into the right errdefs class.
//
// Both helpers preserve the original error chain (errors.Unwrap walks
// through to the original SDK error type), and short-circuit when the
// input already carries any errdefs classification — this lets callers
// pre-classify cases like ctx.Err() (FromContext) without risk of
// double-wrapping or downgrading.

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// HTTPStatusCoder is implemented by errors that carry an HTTP status code.
// Provider SDKs (openai-go, anthropic-sdk-go, ...) typically expose their
// error types this way; ClassifyProviderError checks this interface first
// because a structured status is the most reliable signal.
type HTTPStatusCoder interface {
	HTTPStatusCode() int
}

// ClassifyProviderError wraps an arbitrary provider error in an errdefs
// classification so pod-level policies (retry, circuit breaking, fail-fast
// on auth) can decide on it via the standard IsXxx checks. The original
// error chain is preserved; callers needing finer-grained signals
// (vendor-specific fields, llm.ErrorCategory) can still errors.As against
// the inner type.
//
// Mapping:
//
//	HTTP 401, 403                 → Unauthorized
//	HTTP 402                      → Forbidden     (long-term unusable, not 429)
//	HTTP 429 / "rate limit" / ...  → RateLimit
//	HTTP 400, 404, 405, 422       → Validation    (client error, no retry)
//	"context length exceeded" / ...→ Validation    (input too large; client must shrink)
//	HTTP 5xx / network flake       → NotAvailable  (transient, safe to retry)
//
// Returns nil when err is nil; returns err untouched when it already
// carries an errdefs classification.
func ClassifyProviderError(provider string, err error) error {
	if err == nil {
		return nil
	}
	if HasClassification(err) {
		return err
	}
	wrapped := fmt.Errorf("%s: %w", provider, err)
	switch ClassifyProvider(err) {
	case ProviderAuth:
		return Unauthorized(wrapped)
	case ProviderBilling:
		return Forbidden(wrapped)
	case ProviderRateLimit:
		return RateLimit(wrapped)
	case ProviderContextOverflow, ProviderPermanent:
		return Validation(wrapped)
	default:
		return NotAvailable(wrapped)
	}
}

// ClassifyHTTPStatus wraps a raw HTTP status code into an errdefs-
// classified error. Use this from callers that drive HTTP themselves
// (e.g. ollama, custom REST adapters). body is included verbatim in the
// error message to preserve provider diagnostics; pass an empty string
// when no body is available. Codes below 400 always map to Internal —
// the helper assumes callers have already gated on a non-2xx response.
func ClassifyHTTPStatus(provider string, code int, body string) error {
	msg := fmt.Sprintf("%s: http %d", provider, code)
	if body != "" {
		msg = fmt.Sprintf("%s: %s", msg, body)
	}
	wrapped := errors.New(msg)
	if cat, ok := ProviderCategoryFromHTTPCode(code); ok {
		switch cat {
		case ProviderAuth:
			return Unauthorized(wrapped)
		case ProviderBilling:
			return Forbidden(wrapped)
		case ProviderRateLimit:
			return RateLimit(wrapped)
		case ProviderPermanent:
			return Validation(wrapped)
		}
	}
	if code >= 500 {
		return NotAvailable(wrapped)
	}
	return Internal(wrapped)
}

// ProviderCategory is a finer-grained classification dimension for
// errors coming back from external providers. It carries strictly more
// information than the errdefs IsXxx behavior set: 401/403 (Auth) and
// 402 (Billing) both map to Forbidden/Unauthorized for wire purposes,
// but consumers like sdk/llm.FallbackLLM need to distinguish them when
// choosing a cooldown duration. ContextOverflow and Permanent likewise
// both surface as Validation but warrant different telemetry buckets.
//
// ProviderCategory is exported so sibling capabilities (sdk/llm chat
// fallback, future sdk/embedding rerank chains, ...) share one keyword
// table and one HTTP-code switch. New consumers SHOULD wrap it in a
// domain-specific type alias rather than referencing it directly, so
// the enum name reads naturally at the call site.
type ProviderCategory int

const (
	ProviderTransient       ProviderCategory = iota // network flake, 5xx
	ProviderRateLimit                               // 429
	ProviderAuth                                    // 401 / 403
	ProviderBilling                                 // 402
	ProviderContextOverflow                         // input too large
	ProviderPermanent                               // 400 / 404 / 405 / 422
)

// ProviderCategoryFromHTTPCode maps a non-2xx HTTP status into a
// ProviderCategory. Returns (ProviderTransient, false) for codes that
// have no specific mapping (informational / 3xx / generic 5xx); callers
// SHOULD treat those as transient and retryable.
func ProviderCategoryFromHTTPCode(code int) (ProviderCategory, bool) {
	switch code {
	case 401, 403:
		return ProviderAuth, true
	case 402:
		return ProviderBilling, true
	case 429:
		return ProviderRateLimit, true
	case 400, 404, 405, 422:
		return ProviderPermanent, true
	default:
		return ProviderTransient, false
	}
}

// ClassifyProvider determines a ProviderCategory for an arbitrary
// error coming back from a provider SDK. Use this when you need the
// richer category dimension (e.g. choosing per-category cooldown);
// most call sites should prefer ClassifyProviderError, which folds the
// classification straight into an errdefs marker.
//
// Match order is significant:
//  1. structured HTTPStatusCoder — most reliable, set by SDK error
//     types like openai-go's APIError;
//  2. keyword scan — catches "context length exceeded" before its
//     sibling "http 400" would match as Permanent;
//  3. embedded HTTP code regex — catches providers that only expose
//     the status as part of an error string ("api error: status 429:
//     ...").
func ClassifyProvider(err error) ProviderCategory {
	if err == nil {
		return ProviderTransient
	}

	var coded HTTPStatusCoder
	if errors.As(err, &coded) {
		if cat, ok := ProviderCategoryFromHTTPCode(coded.HTTPStatusCode()); ok {
			return cat
		}
	}

	msg := collectErrorMessages(err)

	for _, c := range providerKeywordClassifiers {
		for _, kw := range c.keywords {
			if strings.Contains(msg, kw) {
				return c.category
			}
		}
	}

	if m := providerHTTPCodePattern.FindStringSubmatch(msg); len(m) == 2 {
		if code, e := strconv.Atoi(m[1]); e == nil {
			if cat, ok := ProviderCategoryFromHTTPCode(code); ok {
				return cat
			}
		}
	}

	return ProviderTransient
}

var providerHTTPCodePattern = regexp.MustCompile(`\b(?:http|status)\s*(\d{3})\b`)

// providerKeywordClassifiers ordered by priority — context-overflow keywords
// are checked before generic "rate limit" / "http 400" matches so that
// "http 400: maximum context length exceeded" classifies as
// ContextOverflow rather than as a generic Permanent. Both still surface
// as Validation in errdefs, but consumers using ProviderCategory
// directly (e.g. sdk/llm fallback for cooldown selection) need the
// finer-grained signal.
var providerKeywordClassifiers = []struct {
	category ProviderCategory
	keywords []string
}{
	{ProviderContextOverflow, []string{
		"context length exceeded",
		"maximum context length",
		"too many tokens",
		"content too large",
		"request too large",
		"payload too large",
		"max_tokens",
	}},
	{ProviderAuth, []string{
		"invalid api key",
		"incorrect api key",
		"unauthorized",
		"authentication",
		"permission denied",
		"api key not found",
		"invalid_api_key",
		"invalid x-api-key",
	}},
	{ProviderBilling, []string{
		"insufficient credits",
		"insufficient credit",
		"billing",
		"payment required",
		"quota exceeded",
		"insufficient_quota",
		"no credit",
	}},
	{ProviderRateLimit, []string{
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

// collectErrorMessages walks the error chain — including multi-errors
// produced by errors.Join and anything implementing Unwrap() []error —
// and concatenates all messages so keyword scanning can hit phrases that
// only appear in a wrapped inner error.
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
