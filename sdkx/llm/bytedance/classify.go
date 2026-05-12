package bytedance

import (
	"errors"

	"github.com/GizClaw/flowcraft/sdk/errdefs"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

// classifyAPIError gives Volcengine arkruntime's *APIError /
// *RequestError types a status-code-aware classification path.
//
// Same root cause as the openai-go and anthropic-sdk-go variants:
// arkruntime's APIError.Error() formats as `"Error code: %d - %s"` and
// RequestError.Error() as `"RequestError code: %d, ..."`. Neither
// string contains the literal "http" or "status" keywords the generic
// [errdefs.ClassifyProvider] regex looks for, so a real 400 falls
// through to the ProviderTransient default → NotAvailable, which the
// eval runner's retry-once would then retry forever instead of
// fail-fast. This package's wrapper routes the codes through their
// proper buckets.
//
// Mapping (HTTPStatusCode → errdefs class) matches the OpenAI variant
// minus the Azure-capacity 404 quirk: ByteDance Ark doesn't share that
// failure mode and a 404 from Volcengine is always a real misconfig.
//
//	401, 403         → Unauthorized
//	402              → Forbidden
//	429              → RateLimit
//	400, 404, 405, 422 → Validation
//	408, 409, >=500  → NotAvailable
func classifyAPIError(err error) error {
	if err == nil {
		return nil
	}
	if errdefs.HasClassification(err) {
		return err
	}
	// Both error types in arkruntime expose HTTPStatusCode as an int
	// field; check both — RequestError wraps the underlying ArkAPIError
	// via Unwrap so errors.As lands either, but the field name and
	// position differ enough that a single switch is clearer.
	var ae *arkmodel.APIError
	if errors.As(err, &ae) {
		return classifyArkStatus(err, ae.HTTPStatusCode)
	}
	var re *arkmodel.RequestError
	if errors.As(err, &re) {
		return classifyArkStatus(err, re.HTTPStatusCode)
	}
	return errdefs.ClassifyProviderError("bytedance", err)
}

func classifyArkStatus(err error, status int) error {
	switch status {
	case 401, 403:
		return errdefs.Unauthorized(err)
	case 402:
		return errdefs.Forbidden(err)
	case 429:
		return errdefs.RateLimit(err)
	case 400, 404, 405, 422:
		return errdefs.Validation(err)
	case 408, 409:
		return errdefs.NotAvailable(err)
	}
	if status >= 500 {
		return errdefs.NotAvailable(err)
	}
	return errdefs.ClassifyProviderError("bytedance", err)
}
