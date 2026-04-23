package speech_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/voice"
	"github.com/GizClaw/flowcraft/voice/provider"
)

func TestClassifyError_Nil(t *testing.T) {
	if got := speech.ClassifyError(nil); got != speech.ErrorCodeUnknown {
		t.Fatalf("ClassifyError(nil) = %q, want unknown", got)
	}
}

func TestClassifyError_ContextDeadlineExceeded(t *testing.T) {
	if got := speech.ClassifyError(context.DeadlineExceeded); got != speech.ErrorCodeTimeout {
		t.Fatalf("got %q, want timeout", got)
	}
}

func TestClassifyError_ContextCanceled(t *testing.T) {
	if got := speech.ClassifyError(context.Canceled); got != speech.ErrorCodeInterrupted {
		t.Fatalf("got %q, want interrupted", got)
	}
}

func TestClassifyError_ClosedPipe(t *testing.T) {
	if got := speech.ClassifyError(io.ErrClosedPipe); got != speech.ErrorCodeTransport {
		t.Fatalf("got %q, want transport_error", got)
	}
}

func TestClassifyError_UntypedTimeout(t *testing.T) {
	err := errors.New("request timeout")
	if got := speech.ClassifyError(err); got != speech.ErrorCodeTimeout {
		t.Fatalf("got %q, want timeout", got)
	}
}

func TestClassifyError_UntypedUnavailable(t *testing.T) {
	err := errors.New("service unavailable 503")
	if got := speech.ClassifyError(err); got != speech.ErrorCodeProviderUnavailable {
		t.Fatalf("got %q, want provider_unavailable", got)
	}
}

func TestClassifyError_UntypedBadAudio(t *testing.T) {
	err := errors.New("unsupported audio codec")
	if got := speech.ClassifyError(err); got != speech.ErrorCodeBadAudio {
		t.Fatalf("got %q, want bad_audio", got)
	}
}

func TestClassifyError_UntypedInternal(t *testing.T) {
	err := errors.New("something unknown happened")
	if got := speech.ClassifyError(err); got != speech.ErrorCodeInternal {
		t.Fatalf("got %q, want internal_error", got)
	}
}

func TestClassifyError_ProviderError(t *testing.T) {
	err := &provider.ProviderError{
		Code:    "provider_unavailable",
		Op:      "tts.synthesize",
		Message: "custom provider error",
	}
	if got := speech.ClassifyError(err); got != speech.ErrorCodeProviderUnavailable {
		t.Fatalf("got %q, want provider_unavailable", got)
	}
}

func TestClassifyError_WrappedProviderError(t *testing.T) {
	inner := &provider.ProviderError{
		Code:    "timeout",
		Op:      "stt.recognize",
		Message: "timed out",
	}
	wrapped := errors.Join(errors.New("outer"), inner)
	if got := speech.ClassifyError(wrapped); got != speech.ErrorCodeTimeout {
		t.Fatalf("got %q, want timeout for wrapped ProviderError", got)
	}
}

func TestExtraKeyConstants(t *testing.T) {
	if speech.ExtraKeyLanguage != "speech.language" {
		t.Fatalf("ExtraKeyLanguage = %q", speech.ExtraKeyLanguage)
	}
	if speech.ExtraKeyEmotion != "speech.emotion" {
		t.Fatalf("ExtraKeyEmotion = %q", speech.ExtraKeyEmotion)
	}
	if speech.ExtraKeyVolume != "speech.volume" {
		t.Fatalf("ExtraKeyVolume = %q", speech.ExtraKeyVolume)
	}
	if speech.ExtraKeyScene != "speech.scene" {
		t.Fatalf("ExtraKeyScene = %q", speech.ExtraKeyScene)
	}
}
