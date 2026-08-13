package anthropic

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/GizClaw/flowcraft/core/errdefs"

	"github.com/anthropics/anthropic-sdk-go"
)

// classifyError normalizes any error returned by the Anthropic SDK into
// the errdefs taxonomy.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if classified := errdefs.FromContext(err); errdefs.HasClassification(classified) {
		return classified
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		classified := classifyHTTPStatus(apiErr.StatusCode, err)
		classified = errdefs.WithRequestID(classified, apiErr.RequestID)
		if apiErr.Response != nil {
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
	return errdefs.NotAvailable(fmt.Errorf("anthropic: %w", err))
}

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
		return errdefs.Validation(fmt.Errorf("anthropic: %w", err))
	case http.StatusUnauthorized:
		return errdefs.Unauthorized(fmt.Errorf("anthropic: %w", err))
	case http.StatusForbidden:
		return errdefs.Forbidden(fmt.Errorf("anthropic: %w", err))
	case http.StatusTooManyRequests:
		return errdefs.RateLimit(fmt.Errorf("anthropic: %w", err))
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return errdefs.Timeout(fmt.Errorf("anthropic: %w", err))
	case http.StatusConflict:
		return errdefs.Conflict(fmt.Errorf("anthropic: %w", err))
	}
	return errdefs.NotAvailable(fmt.Errorf("anthropic: %w", err))
}
