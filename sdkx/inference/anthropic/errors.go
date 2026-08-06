package anthropic

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/GizClaw/flowcraft/sdk/errdefs"

	"github.com/anthropics/anthropic-sdk-go"
)

// classifyError normalizes any error returned by the Anthropic SDK into
// the errdefs taxonomy. The inference runtime preserves this classification
// inside ProviderFailure, so transports return it directly.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if classified := errdefs.FromContext(err); errdefs.HasClassification(classified) {
		return classified
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return classifyHTTPStatus(apiErr.StatusCode, err)
	}
	return errdefs.NotAvailable(fmt.Errorf("anthropic: %w", err))
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
