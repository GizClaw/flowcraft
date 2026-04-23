package tts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/voice/provider"
)

type failingTTS struct{ err error }

func (f failingTTS) Synthesize(context.Context, string, ...TTSOption) (io.ReadCloser, error) {
	return nil, f.err
}

func (f failingTTS) Voices(context.Context) ([]Voice, error) { return nil, f.err }

type okTTS struct{ payload string }

func (o okTTS) Synthesize(context.Context, string, ...TTSOption) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader([]byte(o.payload))), nil
}

func (o okTTS) Voices(context.Context) ([]Voice, error) { return nil, nil }

type flakyTTS struct {
	errs    []error
	payload string
	calls   int
}

func (f *flakyTTS) Synthesize(context.Context, string, ...TTSOption) (io.ReadCloser, error) {
	f.calls++
	if len(f.errs) >= f.calls && f.errs[f.calls-1] != nil {
		return nil, f.errs[f.calls-1]
	}
	return io.NopCloser(bytes.NewReader([]byte(f.payload))), nil
}

func (f *flakyTTS) Voices(context.Context) ([]Voice, error) {
	f.calls++
	if len(f.errs) >= f.calls && f.errs[f.calls-1] != nil {
		return nil, f.errs[f.calls-1]
	}
	return nil, nil
}

func TestFallbackTTS_SynthesizeFallsBack(t *testing.T) {
	tts := NewFallbackTTS(
		failingTTS{err: errors.New("primary down")},
		okTTS{payload: "audio"},
	)

	rc, err := tts.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "audio" {
		t.Fatalf("payload=%q, want audio", string(data))
	}
}

func TestFallbackTTS_RetriesRetryableErrorOnSameProvider(t *testing.T) {
	primary := &flakyTTS{
		errs:    []error{errors.New("503 unavailable")},
		payload: "audio",
	}
	tts := NewFallbackTTSWithPolicy(provider.FallbackPolicy{
		MaxAttempts: 2,
	}, primary)

	rc, err := tts.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "audio" {
		t.Fatalf("payload=%q, want audio", string(data))
	}
	if primary.calls != 2 {
		t.Fatalf("calls=%d, want 2", primary.calls)
	}
}

func TestFallbackTTS_FallsBackOnNonRetryableError(t *testing.T) {
	tts := NewFallbackTTSWithPolicy(provider.FallbackPolicy{
		MaxAttempts: 2,
	}, failingTTS{err: errors.New("unsupported audio codec")}, okTTS{payload: "audio"})

	rc, err := tts.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	_ = rc.Close()
}

func TestFallbackTTS_CircuitBreakerSkipsOpenProvider(t *testing.T) {
	primary := &flakyTTS{errs: []error{
		errors.New("503 unavailable"),
		errors.New("503 unavailable"),
	}}
	secondary := &flakyTTS{payload: "backup"}
	tts := NewFallbackTTSWithPolicy(provider.FallbackPolicy{
		MaxAttempts:       1,
		CircuitBreakAfter: 2,
		CircuitOpen:       time.Minute,
	}, primary, secondary)

	for i := 0; i < 2; i++ {
		rc, err := tts.Synthesize(context.Background(), "hello")
		if err != nil {
			t.Fatalf("Synthesize #%d: %v", i+1, err)
		}
		_ = rc.Close()
	}

	rc, err := tts.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Synthesize after open circuit: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if primary.calls != 2 {
		t.Fatalf("primary calls=%d, want 2 after open circuit skip", primary.calls)
	}
}
