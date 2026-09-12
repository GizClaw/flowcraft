package openai

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/GizClaw/flowcraft/core/errdefs"

	"github.com/openai/openai-go/v3"
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
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		classified := errdefs.ClassifyStatus(
			apiErr.StatusCode, fmt.Errorf("%s: %w", providerID, err),
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
