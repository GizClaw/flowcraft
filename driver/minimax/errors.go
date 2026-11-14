package minimax

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/goccy/go-json"
)

// MiniMax's v1 media surfaces answer most failures with HTTP 200 and a
// base_resp envelope, so the status code inside the body — not the transport
// status — carries the failure signal. The v2 task API answers ordinary status
// codes but may still describe the failure with the same envelope. The mapping
// below follows the platform's error code reference; a code this driver does
// not know falls back to the transport truth rather than to a wrong
// classification.
const (
	statusRateLimited = 1002
	statusAuthFailed  = 1004
	statusNoBalance   = 1008
	statusSensitive   = 1026
	statusBadParams   = 2013
	statusInvalidKey  = 2049
)

// baseResp is the status envelope the media responses carry.
type baseResp struct {
	Code    int    `json:"status_code"`
	Message string `json:"status_msg"`
}

// statusMessageLimit caps how much of the provider's own message rides an error.
// The text is the platform's, it lands in logs and in caller-visible errors, and
// an over-long one must not become the driver's problem.
const statusMessageLimit = 512

// err classifies the envelope: nil on success, an errdefs-classified failure
// otherwise. The inference runtime preserves this classification inside
// ProviderFailure, so transports return it directly.
func (b baseResp) err(operation string) error {
	if b.Code == 0 {
		return nil
	}
	message := boundedMessage(b.Message)
	base := fmt.Errorf(
		"minimax: %s failed: status %d: %s",
		operation, b.Code, message,
	)
	return classifyCode(b.Code, base)
}

// boundedMessage trims the provider's message and caps it by rune, so truncation
// cannot split a UTF-8 sequence.
func boundedMessage(message string) string {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return "no message"
	}
	runes := []rune(trimmed)
	if len(runes) <= statusMessageLimit {
		return trimmed
	}
	return string(runes[:statusMessageLimit]) + "…(truncated)"
}

// classifyCode maps a platform status code onto an errdefs class. A code this
// driver does not know returns err unchanged: the caller's classification — the
// transport truth on the HTTP path, the provider-failure class on the
// 200-with-envelope path — is kept rather than overwritten with a guess.
func classifyCode(code int, err error) error {
	switch code {
	case statusRateLimited:
		return errdefs.RateLimit(err)
	case statusAuthFailed, statusInvalidKey:
		return errdefs.Unauthorized(err)
	case statusNoBalance:
		return errdefs.Forbidden(err)
	case statusSensitive, statusBadParams:
		return errdefs.Validation(err)
	default:
		return err
	}
}

// inferenceResponseError classifies an undecodable response body. The response
// arrived but its shape is not the one the surface promises, so the failure is
// the provider's, not the caller's.
func inferenceResponseError(endpoint string, err error) error {
	return inference.NewError(
		inference.InvalidProviderResponse, inference.FieldOutput,
		fmt.Errorf("minimax: decode %s response: %w", endpoint, err),
	)
}

// classifyTransport classifies an HTTP failure, refining it with an in-body
// envelope when the body carries one: the v2 API answers with a status code but
// may still describe the failure with a base_resp.
func classifyTransport(status int, body []byte, err error) error {
	classified := errdefs.ClassifyStatus(status, err)
	if len(body) == 0 {
		return classified
	}
	var envelope struct {
		BaseResp baseResp `json:"base_resp"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.BaseResp.Code == 0 {
		return classified
	}
	return classifyCode(envelope.BaseResp.Code, classified)
}
