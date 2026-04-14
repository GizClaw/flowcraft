package speech

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/sdk/speech/provider"
)

type ErrorCode string

const (
	ErrorCodeUnknown             ErrorCode = "unknown"
	ErrorCodeTimeout             ErrorCode = "timeout"
	ErrorCodeProviderUnavailable ErrorCode = "provider_unavailable"
	ErrorCodeBadAudio            ErrorCode = "bad_audio"
	ErrorCodeTransport           ErrorCode = "transport_error"
	ErrorCodeInterrupted         ErrorCode = "interrupted"
	ErrorCodeInternal            ErrorCode = "internal_error"
)

func ClassifyError(err error) ErrorCode {
	if err == nil {
		return ErrorCodeUnknown
	}
	var ce provider.ClassifiedError
	if errors.As(err, &ce) {
		return ErrorCode(ce.ErrorCode())
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ErrorCodeTimeout
	case errors.Is(err, context.Canceled):
		return ErrorCodeInterrupted
	}
	return ErrorCode(provider.ClassifyByMessage(err.Error()))
}
