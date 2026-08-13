package deepseek

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/GizClaw/flowcraft/core/errdefs"

	openaigo "github.com/openai/openai-go/v3"
)

// classifyError normalizes any error returned by the OpenAI SDK transport
// into the errdefs taxonomy. The inference runtime preserves this
// classification inside ProviderFailure, so transports return it directly.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if classified := errdefs.FromContext(err); errdefs.HasClassification(classified) {
		return classified
	}
	if apiErr, ok := errors.AsType[*openaigo.Error](err); ok {
		classified := classifyHTTPStatus(apiErr.StatusCode, err)
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
	return errdefs.NotAvailable(fmt.Errorf("deepseek: %w", err))
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

func classifyHTTPStatus(status int, err error) error {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return errdefs.Validation(fmt.Errorf("deepseek: %w", err))
	case http.StatusUnauthorized:
		return errdefs.Unauthorized(fmt.Errorf("deepseek: %w", err))
	case http.StatusForbidden:
		return errdefs.Forbidden(fmt.Errorf("deepseek: %w", err))
	case http.StatusTooManyRequests:
		return errdefs.RateLimit(fmt.Errorf("deepseek: %w", err))
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return errdefs.Timeout(fmt.Errorf("deepseek: %w", err))
	case http.StatusConflict:
		return errdefs.Conflict(fmt.Errorf("deepseek: %w", err))
	}
	return errdefs.NotAvailable(fmt.Errorf("deepseek: %w", err))
}
