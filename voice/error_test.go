package voice_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/voice"
	"github.com/GizClaw/flowcraft/voice/provider"
)

func TestClassifyError_Nil(t *testing.T) {
	if got := voice.ClassifyError(nil); got != voice.ErrorCodeUnknown {
		t.Fatalf("ClassifyError(nil) = %q, want unknown", got)
	}
}

func TestClassifyError_ContextDeadlineExceeded(t *testing.T) {
	if got := voice.ClassifyError(context.DeadlineExceeded); got != voice.ErrorCodeTimeout {
		t.Fatalf("got %q, want timeout", got)
	}
}

func TestClassifyError_ContextCanceled(t *testing.T) {
	if got := voice.ClassifyError(context.Canceled); got != voice.ErrorCodeInterrupted {
		t.Fatalf("got %q, want interrupted", got)
	}
}

func TestClassifyError_ClosedPipe(t *testing.T) {
	if got := voice.ClassifyError(io.ErrClosedPipe); got != voice.ErrorCodeTransport {
		t.Fatalf("got %q, want transport_error", got)
	}
}

func TestClassifyError_UntypedTimeout(t *testing.T) {
	err := errors.New("request timeout")
	if got := voice.ClassifyError(err); got != voice.ErrorCodeTimeout {
		t.Fatalf("got %q, want timeout", got)
	}
}

func TestClassifyError_UntypedUnavailable(t *testing.T) {
	err := errors.New("service unavailable 503")
	if got := voice.ClassifyError(err); got != voice.ErrorCodeProviderUnavailable {
		t.Fatalf("got %q, want provider_unavailable", got)
	}
}

func TestClassifyError_UntypedBadAudio(t *testing.T) {
	err := errors.New("unsupported audio codec")
	if got := voice.ClassifyError(err); got != voice.ErrorCodeBadAudio {
		t.Fatalf("got %q, want bad_audio", got)
	}
}

func TestClassifyError_UntypedInternal(t *testing.T) {
	err := errors.New("something unknown happened")
	if got := voice.ClassifyError(err); got != voice.ErrorCodeInternal {
		t.Fatalf("got %q, want internal_error", got)
	}
}

func TestClassifyError_ProviderError(t *testing.T) {
	err := &provider.ProviderError{
		Code:    "provider_unavailable",
		Op:      "tts.synthesize",
		Message: "custom provider error",
	}
	if got := voice.ClassifyError(err); got != voice.ErrorCodeProviderUnavailable {
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
	if got := voice.ClassifyError(wrapped); got != voice.ErrorCodeTimeout {
		t.Fatalf("got %q, want timeout for wrapped ProviderError", got)
	}
}

func TestExtraKeyConstants(t *testing.T) {
	if voice.ExtraKeyLanguage != "speech.language" {
		t.Fatalf("ExtraKeyLanguage = %q", voice.ExtraKeyLanguage)
	}
	if voice.ExtraKeyEmotion != "speech.emotion" {
		t.Fatalf("ExtraKeyEmotion = %q", voice.ExtraKeyEmotion)
	}
	if voice.ExtraKeyVolume != "speech.volume" {
		t.Fatalf("ExtraKeyVolume = %q", voice.ExtraKeyVolume)
	}
	if voice.ExtraKeyScene != "speech.scene" {
		t.Fatalf("ExtraKeyScene = %q", voice.ExtraKeyScene)
	}
}
