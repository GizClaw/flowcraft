package minimax

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// MiniMax's media APIs (t2a, video, image) answer every request — including
// failures — with HTTP 200 and a base_resp envelope, so status codes carry
// the failure signal. The mapping below follows the platform's error code
// reference.
const (
	statusRateLimited = 1002
	statusAuthFailed  = 1004
	statusNoBalance   = 1008
	statusSensitive   = 1026
	statusBadParams   = 2013
	statusInvalidKey  = 2049
)

// baseResp is the status envelope every MiniMax media response carries.
type baseResp struct {
	Code    int    `json:"status_code"`
	Message string `json:"status_msg"`
}

// err classifies the envelope: nil on success, an errdefs-classified
// failure otherwise. The inference runtime preserves this classification
// inside ProviderFailure, so transports return it directly.
func (b baseResp) err(operation string) error {
	if b.Code == 0 {
		return nil
	}
	err := fmt.Errorf(
		"minimax: %s failed: status %d: %s",
		operation,
		b.Code,
		b.Message,
	)
	switch b.Code {
	case statusRateLimited:
		return errdefs.RateLimit(err)
	case statusAuthFailed, statusInvalidKey:
		return errdefs.Unauthorized(err)
	case statusNoBalance:
		return errdefs.Forbidden(err)
	case statusSensitive, statusBadParams:
		return errdefs.Validation(err)
	default:
		return errdefs.NotAvailable(err)
	}
}
