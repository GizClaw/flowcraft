package bytedance

import (
	"errors"
	"fmt"

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
