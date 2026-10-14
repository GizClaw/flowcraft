package openai

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/core/errdefs"

	"github.com/openai/openai-go/v3"
)

// providerDetailLimit bounds the provider text one error message carries.
// It rides logs, telemetry attributes, and session events, and a provider is
// free to answer a refusal with a validation dump instead of a sentence.
const providerDetailLimit = 512

// classifyError normalizes any error returned by the OpenAI SDK or the
// realtime WebSocket into the errdefs taxonomy. The inference runtime
// preserves this classification inside ProviderFailure, so transports return
// it directly.
//
// An API error is rendered with the provider's own envelope in its text: see
// providerError.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if classified := errdefs.FromContext(err); errdefs.HasClassification(classified) {
		return classified
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		classified := errdefs.ClassifyStatus(
			apiErr.StatusCode,
			&providerError{text: providerSummary(apiErr), err: err},
		)
		if apiErr.Response != nil {
			classified = errdefs.WithRequestID(
				classified,
				apiErr.Response.Header.Get("x-request-id"),
			)
			classified = errdefs.WithRetryAfter(
				classified,
				errdefs.ParseRetryAfter(apiErr.Response.Header.Get("Retry-After")),
			)
		}
		if attempts := wireAttempts(apiErr.Request); attempts > 0 {
			classified = errdefs.WithRetryCount(classified, attempts)
		}
		return classified
	}
	return errdefs.NotAvailable(fmt.Errorf("openai: %w", err))
}

// providerError carries the provider's own account of a failed call while
// keeping the SDK error reachable through errors.As.
type providerError struct {
	text string
	err  error
}

func (e *providerError) Error() string { return e.text }
func (e *providerError) Unwrap() error { return e.err }

// providerSummary renders one failed API call: the HTTP status, then the
// provider's envelope when the response carried one.
//
// The SDK's own Error() reports the status alone — "OpenAI API error: 403
// Forbidden" — because the value also holds the request URL and the raw
// response body, and the SDK cannot know which caller ends up logging the
// text. The envelope is a different thing: the SDK parses it into typed
// fields, and for a gateway those fields are the whole answer: a refusal
// spelled model_access_denied, invalid_decision_request, or
// insufficient_quota is invisible behind a bare 403, leaving an operator
// with nothing to fix. Sibling drivers name the provider's code and message
// the same way (dashscope's envelope, typesafe's bounded body); this one
// keeps that promise without reaching for the raw JSON.
func providerSummary(apiErr *openai.Error) string {
	summary := fmt.Sprintf("%s: HTTP %d", providerID, apiErr.StatusCode)
	if detail := providerDetail(apiErr); detail != "" {
		summary += ": " + detail
	}
	return summary
}

// providerDetail joins the envelope's code and message into one line, or
// returns "" when the response carried neither — a gateway's HTML error page,
// a body that is not the envelope the SDK parses.
//
// A message that already names its code keeps it once rather than twice.
// Both fields are collapsed to a single line and the joined text is bounded,
// so one provider failure stays one bounded line wherever it is written.
func providerDetail(apiErr *openai.Error) string {
	code := oneLine(apiErr.Code)
	message := oneLine(apiErr.Message)
	detail := ""
	switch {
	case code == "":
		detail = message
	case message == "":
		detail = code
	case strings.Contains(message, code):
		detail = message
	default:
		detail = code + ": " + message
	}
	return boundDetail(detail)
}

// oneLine folds every whitespace or control run into a single space, so a
// multi-line provider message does not break the log line it lands in.
func oneLine(value string) string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
	return strings.Join(fields, " ")
}

// boundDetail truncates provider text to providerDetailLimit runes, marking
// the cut so a truncated message is not mistaken for the whole answer.
func boundDetail(value string) string {
	if runes := []rune(value); len(runes) > providerDetailLimit {
		return string(runes[:providerDetailLimit]) + "..."
	}
	return value
}

// wireAttempts derives the total HTTP sends from the SDK's
// X-Stainless-Retry-Count header (zero-based retry count on the final
// request). Zero means the count was unavailable.
func wireAttempts(request *http.Request) int {
	if request == nil {
		return 0
	}
	value := request.Header.Get("X-Stainless-Retry-Count")
	if value == "" {
		return 0
	}
	return errdefs.ParseRetryCount(value) + 1
}
