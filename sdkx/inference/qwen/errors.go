package qwen

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// classifyHTTPError maps a failed DashScope response (HTTP status plus the
// body's code/message pair) into the errdefs taxonomy. The inference
// runtime preserves this classification inside ProviderFailure, so
// transports return it directly.
func classifyHTTPError(status int, code, message string, raw []byte) error {
	err := fmt.Errorf(
		"qwen: HTTP %d: %s %s: %s",
		status, code, message, strings.TrimSpace(string(raw)),
	)
	if classified := classifyCode(code, err); classified != nil {
		return classified
	}
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return errdefs.Validation(err)
	case http.StatusUnauthorized:
		return errdefs.Unauthorized(err)
	case http.StatusForbidden:
		return errdefs.Forbidden(err)
	case http.StatusTooManyRequests:
		return errdefs.RateLimit(err)
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return errdefs.Timeout(err)
	}
	return errdefs.NotAvailable(err)
}

// classifyCode maps DashScope's string error codes (see the Model Studio
// error-code reference) onto errdefs; unknown codes fall through.
func classifyCode(code string, err error) error {
	switch {
	case strings.Contains(code, "Throttling"), strings.Contains(code, "RateLimit"),
		strings.Contains(code, "Quota"):
		return errdefs.RateLimit(err)
	case strings.Contains(code, "ApiKey"), strings.Contains(code, "Unauthorized"),
		strings.Contains(code, "AccessDenied"):
		return errdefs.Unauthorized(err)
	case strings.Contains(code, "Arrearage"):
		return errdefs.Forbidden(err)
	case strings.Contains(code, "InvalidParameter"), strings.Contains(code, "Illegal"),
		strings.Contains(code, "BadRequest"), strings.Contains(code, "DataInspection"),
		strings.Contains(code, "InputRequired"):
		return errdefs.Validation(err)
	}
	return nil
}

// classifyEnvelope checks the code/message pair on a 200 response: SSE
// streams and edge gateways can report failures with an HTTP-200 envelope
// whose code is non-empty.
func classifyEnvelope(code, message string) error {
	if code == "" {
		return nil
	}
	err := fmt.Errorf("qwen: %s: %s", code, message)
	if classified := classifyCode(code, err); classified != nil {
		return classified
	}
	return errdefs.NotAvailable(err)
}
