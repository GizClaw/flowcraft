package bytedance

import (
	"errors"
	"fmt"
	"strings"

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
			errdefs.ClassifyStatus(
				apiErr.HTTPStatusCode, fmt.Errorf("%s: %w", providerID, err),
			),
			apiErr.RequestId,
		)
	}
	var reqErr *arkmodel.RequestError
	if errors.As(err, &reqErr) {
		return errdefs.WithRequestID(
			errdefs.ClassifyStatus(
				reqErr.HTTPStatusCode, fmt.Errorf("%s: %w", providerID, reqErr.Err),
			),
			reqErr.RequestId,
		)
	}
	return errdefs.NotAvailable(fmt.Errorf("bytedance: %w", err))
}

// classifyResponseError maps an Ark failure the media surfaces carry in the
// response body onto the contract's categories. The platform reports these as
// text, so the mapping reads the text: a rate, an auth or a content refusal is
// what the caller needs to distinguish, and everything else is the provider
// being unavailable rather than the request being wrong.
func classifyResponseError(code, message string) error {
	err := fmt.Errorf("bytedance: response failed: %s %s", code, message)
	lower := strings.ToLower(code + " " + message)
	switch {
	case strings.Contains(lower, "rate"):
		return errdefs.RateLimit(err)
	case strings.Contains(lower, "auth"), strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "permission"):
		return errdefs.Unauthorized(err)
	case strings.Contains(lower, "filter"), strings.Contains(lower, "invalid"),
		strings.Contains(lower, "notfound"):
		return errdefs.Validation(err)
	default:
		return errdefs.NotAvailable(err)
	}
}
