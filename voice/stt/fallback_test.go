package stt

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/speech/audio"
	"github.com/GizClaw/flowcraft/sdk/speech/provider"
)

type failingSTT struct{ err error }

func (f failingSTT) Recognize(context.Context, audio.Frame, ...STTOption) (STTResult, error) {
	return STTResult{}, f.err
}

type okSTT struct{ text string }

func (o okSTT) Recognize(context.Context, audio.Frame, ...STTOption) (STTResult, error) {
	return STTResult{Text: o.text, IsFinal: true}, nil
}

type flakySTT struct {
	errs  []error
	text  string
	calls int
}

func (f *flakySTT) Recognize(context.Context, audio.Frame, ...STTOption) (STTResult, error) {
	f.calls++
	if len(f.errs) >= f.calls && f.errs[f.calls-1] != nil {
		return STTResult{}, f.errs[f.calls-1]
	}
	return STTResult{Text: f.text, IsFinal: true}, nil
}

func TestFallbackSTT_RecognizeFallsBack(t *testing.T) {
	stt := NewFallbackSTT(
		failingSTT{err: errors.New("primary down")},
		okSTT{text: "hello"},
	)

	res, err := stt.Recognize(context.Background(), audio.Frame{})
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if res.Text != "hello" {
		t.Fatalf("Text=%q, want hello", res.Text)
	}
}

func TestFallbackSTT_RetriesRetryableErrorOnSameProvider(t *testing.T) {
	primary := &flakySTT{
		errs: []error{errors.New("503 unavailable")},
		text: "hello",
	}
	stt := NewFallbackSTTWithPolicy(provider.FallbackPolicy{
		MaxAttempts: 2,
	}, primary)

	res, err := stt.Recognize(context.Background(), audio.Frame{})
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if res.Text != "hello" {
		t.Fatalf("Text=%q, want hello", res.Text)
	}
	if primary.calls != 2 {
		t.Fatalf("calls=%d, want 2", primary.calls)
	}
}

func TestFallbackSTT_FallsBackOnNonRetryableError(t *testing.T) {
	stt := NewFallbackSTTWithPolicy(provider.FallbackPolicy{
		MaxAttempts: 2,
	}, failingSTT{err: errors.New("unsupported audio codec")}, okSTT{text: "hello"})

	res, err := stt.Recognize(context.Background(), audio.Frame{})
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if res.Text != "hello" {
		t.Fatalf("Text=%q, want hello", res.Text)
	}
}

func TestFallbackSTT_CircuitBreakerSkipsOpenProvider(t *testing.T) {
	primary := &flakySTT{errs: []error{
		errors.New("503 unavailable"),
		errors.New("503 unavailable"),
	}}
	secondary := &flakySTT{text: "backup"}
	stt := NewFallbackSTTWithPolicy(provider.FallbackPolicy{
		MaxAttempts:       1,
		CircuitBreakAfter: 2,
		CircuitOpen:       time.Minute,
	}, primary, secondary)

	for i := 0; i < 2; i++ {
		res, err := stt.Recognize(context.Background(), audio.Frame{})
		if err != nil {
			t.Fatalf("Recognize #%d: %v", i+1, err)
		}
		if res.Text != "backup" {
			t.Fatalf("Text=%q, want backup", res.Text)
		}
	}

	res, err := stt.Recognize(context.Background(), audio.Frame{})
	if err != nil {
		t.Fatalf("Recognize after open circuit: %v", err)
	}
	if res.Text != "backup" {
		t.Fatalf("Text=%q, want backup", res.Text)
	}
	if primary.calls != 2 {
		t.Fatalf("primary calls=%d, want 2 after open circuit skip", primary.calls)
	}
}
