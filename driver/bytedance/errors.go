package bytedance

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/GizClaw/flowcraft/core/errdefs"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

// classifyError normalizes any error returned by the Ark SDK into the errdefs
// taxonomy. The inference runtime preserves this classification inside
// ProviderFailure, so transports return it directly.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if classified := errdefs.FromContext(err); errdefs.HasClassification(classified) {
		return classified
	}
	var apiErr *arkmodel.APIError
	if errors.As(err, &apiErr) {
		return errdefs.WithRequestID(
			classifyHTTPStatus(
				apiErr.HTTPStatusCode, apiErr.Code, apiErr.Message, err,
			),
			apiErr.RequestId,
		)
	}
	var reqErr *arkmodel.RequestError
	if errors.As(err, &reqErr) {
		return errdefs.WithRequestID(
			classifyHTTPStatus(reqErr.HTTPStatusCode, "", "", reqErr.Err),
			reqErr.RequestId,
		)
	}
	return errdefs.NotAvailable(fmt.Errorf("bytedance: %w", err))
}

func classifyHTTPStatus(status int, code, message string, err error) error {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return errdefs.Validation(fmt.Errorf("bytedance: %w", err))
	case http.StatusUnauthorized:
		return errdefs.Unauthorized(fmt.Errorf("bytedance: %w", err))
	case http.StatusForbidden:
		return errdefs.Forbidden(fmt.Errorf("bytedance: %w", err))
	case http.StatusTooManyRequests:
		return errdefs.RateLimit(fmt.Errorf("bytedance: %w", err))
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return errdefs.Timeout(fmt.Errorf("bytedance: %w", err))
	case http.StatusConflict:
		return errdefs.Conflict(fmt.Errorf("bytedance: %w", err))
	}
	if status >= 500 {
		return errdefs.NotAvailable(fmt.Errorf("bytedance: %w", err))
	}
	return errdefs.NotAvailable(fmt.Errorf("bytedance: %s %s: %w", code, message, err))
}
