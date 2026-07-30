package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// errorEnvelope is Kimi's failure body: {"error": {message, type, code}}.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// classifyError normalizes transport-level errors into the errdefs
// taxonomy. The inference runtime preserves this classification inside
// ProviderFailure, so transports return it directly.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if classified := errdefs.FromContext(err); errdefs.HasClassification(classified) {
		return classified
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errdefs.Timeout(fmt.Errorf("kimi: %w", err))
	}
	if netErr, ok := errors.AsType[net.Error](err); ok {
		if netErr.Timeout() {
			return errdefs.Timeout(fmt.Errorf("kimi: %w", err))
		}
		return errdefs.NotAvailable(fmt.Errorf("kimi: %w", err))
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errdefs.NotAvailable(fmt.Errorf("kimi: %w", err))
	}
	return errdefs.NotAvailable(fmt.Errorf("kimi: %w", err))
}

// classifyHTTPError classifies a non-2xx response from its status plus
// the error envelope's type/code pair.
func classifyHTTPError(status int, body []byte) error {
	var envelope errorEnvelope
	_ = json.Unmarshal(body, &envelope)
	message := envelope.Error.Message
	if message == "" {
		message = http.StatusText(status)
	}
	kind := envelope.Error.Type
	if kind == "" {
		kind = envelope.Error.Code
	}
	detail := fmt.Errorf("kimi: HTTP %d: %s", status, message)

	switch status {
	case http.StatusBadRequest:
		// Kimi marks request-shape failures invalid_request_error; a
		// missing model surfaces as a 400 naming the model.
		if strings.Contains(kind, "invalid") || kind == "" {
			return errdefs.Validation(detail)
		}
		return classifyKind(kind, detail)
	case http.StatusUnauthorized:
		return errdefs.Unauthorized(detail)
	case http.StatusForbidden:
		return errdefs.Forbidden(detail)
	case http.StatusNotFound:
		return errdefs.Validation(detail)
	case http.StatusTooManyRequests:
		return errdefs.RateLimit(detail)
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return errdefs.Timeout(detail)
	case http.StatusConflict:
		return errdefs.Conflict(detail)
	}
	if status >= 500 {
		return errdefs.NotAvailable(detail)
	}
	return errdefs.NotAvailable(detail)
}

// classifyKind maps Kimi's error.type vocabulary when the status alone
// does not decide.
func classifyKind(kind string, detail error) error {
	switch {
	case strings.Contains(kind, "auth"), strings.Contains(kind, "permission"):
		return errdefs.Unauthorized(detail)
	case strings.Contains(kind, "rate_limit"), strings.Contains(kind, "throttl"):
		return errdefs.RateLimit(detail)
	case strings.Contains(kind, "invalid"), strings.Contains(kind, "validation"):
		return errdefs.Validation(detail)
	case strings.Contains(kind, "timeout"):
		return errdefs.Timeout(detail)
	case strings.Contains(kind, "server"), strings.Contains(kind, "engine_overloaded"):
		return errdefs.NotAvailable(detail)
	}
	return errdefs.NotAvailable(detail)
}
