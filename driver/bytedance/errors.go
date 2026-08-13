package bytedance

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/GizClaw/flowcraft/core/errdefs"

	doubaospeech "github.com/GizClaw/doubao-speech-go"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

// classifyError normalizes any error returned by the Ark or speech SDKs into
// the errdefs taxonomy. The inference runtime preserves this classification
// inside ProviderFailure, so transports return it directly.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if classified := errdefs.FromContext(err); errdefs.HasClassification(classified) {
		return classified
	}
	if apiErr, ok := errors.AsType[*arkmodel.APIError](err); ok {
		return errdefs.WithRequestID(
			classifyHTTPStatus(
				apiErr.HTTPStatusCode, apiErr.Code, apiErr.Message, err,
			),
			apiErr.RequestId,
		)
	}
	if reqErr, ok := errors.AsType[*arkmodel.RequestError](err); ok {
		return errdefs.WithRequestID(
			classifyHTTPStatus(reqErr.HTTPStatusCode, "", "", reqErr.Err),
			reqErr.RequestId,
		)
	}
	if speechErr, ok := errors.AsType[*doubaospeech.Error](err); ok {
		return classifySpeechError(speechErr, err)
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

func classifySpeechError(speechErr *doubaospeech.Error, err error) error {
	var classified error
	switch {
	case speechErr.IsAuthError():
		if speechErr.HTTPStatus == http.StatusForbidden {
			classified = errdefs.Forbidden(fmt.Errorf("bytedance: %w", err))
		} else {
			classified = errdefs.Unauthorized(fmt.Errorf("bytedance: %w", err))
		}
	case speechErr.IsRateLimit(), speechErr.IsQuotaExceeded():
		classified = errdefs.RateLimit(fmt.Errorf("bytedance: %w", err))
	case speechErr.IsInvalidParam():
		classified = errdefs.Validation(fmt.Errorf("bytedance: %w", err))
	case speechErr.IsServerError():
		classified = errdefs.NotAvailable(fmt.Errorf("bytedance: %w", err))
	default:
		classified = errdefs.NotAvailable(fmt.Errorf("bytedance: %w", err))
	}
	return errdefs.WithRequestID(classified, speechErr.ReqID)
}
