package openai

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/GizClaw/flowcraft/sdk/errdefs"

	"github.com/openai/openai-go"
)

// classifyError normalizes any error returned by the OpenAI SDK or the
// realtime WebSocket into the errdefs taxonomy. The inference runtime
// preserves this classification inside ProviderFailure, so transports return
// it directly.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if classified := errdefs.FromContext(err); errdefs.HasClassification(classified) {
		return classified
	}
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		return classifyHTTPStatus(apiErr.StatusCode, err)
	}
	return errdefs.NotAvailable(fmt.Errorf("openai: %w", err))
}

func classifyHTTPStatus(status int, err error) error {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return errdefs.Validation(fmt.Errorf("openai: %w", err))
	case http.StatusUnauthorized:
		return errdefs.Unauthorized(fmt.Errorf("openai: %w", err))
	case http.StatusForbidden:
		return errdefs.Forbidden(fmt.Errorf("openai: %w", err))
	case http.StatusTooManyRequests:
		return errdefs.RateLimit(fmt.Errorf("openai: %w", err))
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return errdefs.Timeout(fmt.Errorf("openai: %w", err))
	case http.StatusConflict:
		return errdefs.Conflict(fmt.Errorf("openai: %w", err))
	}
	if status >= 500 {
		return errdefs.NotAvailable(fmt.Errorf("openai: %w", err))
	}
	return errdefs.NotAvailable(fmt.Errorf("openai: %w", err))
}
