package stt_test

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/speech/audio"
	"github.com/GizClaw/flowcraft/sdk/speech/stt"
)

// ---------------------------------------------------------------------------
// Eager test fakes (prefixed to avoid conflict with pipeline_test fakes)
// ---------------------------------------------------------------------------

// eagerFakeSTT returns preset results in sequence for Recognize.
type eagerFakeSTT struct {
	results []stt.STTResult
	callIdx int
}

func (f *eagerFakeSTT) Recognize(ctx context.Context, input audio.Frame, opts ...stt.STTOption) (stt.STTResult, error) {
	if f.callIdx >= len(f.results) {
		return stt.STTResult{Text: "overflow", IsFinal: true}, nil
	}
	r := f.results[f.callIdx]
	f.callIdx++
	// Copy so caller can't mutate our preset
	return stt.STTResult{
		Text:    r.Text,
		Audio:   input, // EagerRecognizer overwrites Audio with its own copy
		IsFinal: r.IsFinal,
	}, nil
}

// eagerFakeVAD returns segments on specific Feed calls.
type eagerFakeVAD struct {
	responses []struct {
		segment []byte
		isFinal bool
	}
	feedIdx int
	flush   []byte
}

func (v *eagerFakeVAD) Feed(chunk []byte) (segment []byte, isFinal bool) {
	if v.feedIdx >= len(v.responses) {
		return nil, false
	}
	r := v.responses[v.feedIdx]
	v.feedIdx++
	return r.segment, r.isFinal
}

func (v *eagerFakeVAD) Reset() {}

func (v *eagerFakeVAD) Flush() []byte {
	return v.flush
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestEagerRecognizer_Recognize(t *testing.T) {
	sttImpl := &eagerFakeSTT{results: []stt.STTResult{
		{Text: "hello", IsFinal: true},
	}}
	vad := &eagerFakeVAD{}
	rec := stt.NewEagerRecognizer(sttImpl, vad)

	ctx := context.Background()
	frame := audio.Frame{
		Data:   []byte("audio-data"),
		Format: audio.Format{Codec: audio.CodecPCM, SampleRate: 16000, Channels: 1, BitDepth: 16},
	}

	result, err := rec.Recognize(ctx, frame)
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if result.Text != "hello" {
		t.Errorf("Text = %q, want hello", result.Text)
	}
	if !result.IsFinal {
		t.Error("IsFinal = false, want true")
	}
	if !bytes.Equal(result.Audio.Data, frame.Data) {
		t.Errorf("Audio.Data = %q, want %q", result.Audio.Data, frame.Data)
	}
}

func TestEagerRecognizer_RecognizeStream(t *testing.T) {
	sttImpl := &eagerFakeSTT{results: []stt.STTResult{
		{Text: "first", IsFinal: false},
		{Text: "second", IsFinal: true},
	}}
	vad := &eagerFakeVAD{
		responses: []struct {
			segment []byte
			isFinal bool
		}{
			{[]byte("chunk1"), false},
			{[]byte("chunk2"), true},
		},
	}

	rec := stt.NewEagerRecognizer(sttImpl, vad)
	input := audio.NewPipe[audio.Frame](4)
	fmt := audio.Format{Codec: audio.CodecPCM, SampleRate: 16000, Channels: 1, BitDepth: 16}

	input.Send(audio.Frame{Data: []byte("a"), Format: fmt})
	input.Send(audio.Frame{Data: []byte("b"), Format: fmt})
	input.Close()

	ctx := context.Background()
	stream, err := rec.RecognizeStream(ctx, input)
	if err != nil {
		t.Fatalf("RecognizeStream: %v", err)
	}

	var results []stt.STTResult
	for {
		r, err := stream.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		results = append(results, r)
	}

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Text != "first" || results[0].IsFinal {
		t.Errorf("result[0] = {Text: %q, IsFinal: %v}, want {first, false}", results[0].Text, results[0].IsFinal)
	}
	if results[1].Text != "second" || !results[1].IsFinal {
		t.Errorf("result[1] = {Text: %q, IsFinal: %v}, want {second, true}", results[1].Text, results[1].IsFinal)
	}
}

// TestEagerRecognizer_ContextCancellation simulates the production shutdown
// sequence: session cancels ctx and interrupts the input pipe.
func TestEagerRecognizer_ContextCancellation(t *testing.T) {
	sttImpl := &eagerFakeSTT{results: []stt.STTResult{{Text: "x", IsFinal: false}}}
	vad := &eagerFakeVAD{
		responses: []struct {
			segment []byte
			isFinal bool
		}{
			{[]byte("chunk"), false},
		},
	}
	rec := stt.NewEagerRecognizer(sttImpl, vad)

	ctx, cancel := context.WithCancel(context.Background())
	input := audio.NewPipe[audio.Frame](4)
	fmt := audio.Format{Codec: audio.CodecPCM, SampleRate: 16000}

	input.Send(audio.Frame{Data: []byte("a"), Format: fmt})
	input.Send(audio.Frame{Data: []byte("b"), Format: fmt})

	stream, err := rec.RecognizeStream(ctx, input)
	if err != nil {
		t.Fatalf("RecognizeStream: %v", err)
	}

	cancel()
	input.Interrupt()

	var lastErr error
	for {
		_, err := stream.Read()
		if err != nil {
			lastErr = err
			break
		}
	}
	if lastErr != io.EOF && lastErr != context.Canceled {
		t.Errorf("expected EOF or context.Canceled, got %v", lastErr)
	}
}

// TestEagerRecognizer_ContextCancel_OutputUnblocks verifies that the output
// stream unblocks immediately when ctx is cancelled, even if nobody interrupts
// the input. This is the key behavior added by binding out to ctx.
func TestEagerRecognizer_ContextCancel_OutputUnblocks(t *testing.T) {
	// Use a VAD that returns no segments so the goroutine loops back to
	// input.Read() without sending anything to out. This guarantees the
	// goroutine is blocked on input.Read() when we cancel ctx.
	sttImpl := &eagerFakeSTT{}
	vad := &eagerFakeVAD{} // no responses → Feed always returns nil

	rec := stt.NewEagerRecognizer(sttImpl, vad)

	ctx, cancel := context.WithCancel(context.Background())
	input := audio.NewPipe[audio.Frame](4)
	fmt := audio.Format{Codec: audio.CodecPCM, SampleRate: 16000}

	// Send one frame so the goroutine starts processing, then it will
	// block on the second input.Read() because the pipe is empty and open.
	input.Send(audio.Frame{Data: []byte("a"), Format: fmt})

	stream, err := rec.RecognizeStream(ctx, input)
	if err != nil {
		t.Fatalf("RecognizeStream: %v", err)
	}

	// Give the goroutine time to consume the frame and block on Read.
	time.Sleep(10 * time.Millisecond)

	cancel()

	// stream.Read() must return within a reasonable time even though
	// input is still open, because out is interrupted via AfterFunc.
	done := make(chan error, 1)
	go func() {
		_, err := stream.Read()
		done <- err
	}()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream.Read() did not unblock after ctx cancellation")
	}

	// Clean up: interrupt input so the internal goroutine can exit.
	input.Interrupt()
}

// TestEagerRecognizer_ContextCancel_SendReturnsFalse verifies that the
// goroutine exits cleanly when ctx is cancelled while it is producing results,
// because out.Send returns false on an interrupted pipe.
func TestEagerRecognizer_ContextCancel_SendReturnsFalse(t *testing.T) {
	var mu sync.Mutex
	recognizeCalls := 0

	blockingSTT := &eagerFakeSTT{results: []stt.STTResult{
		{Text: "r1", IsFinal: false},
		{Text: "r2", IsFinal: false},
		{Text: "r3", IsFinal: false},
	}}

	countingSTT := &countingSTTWrapper{inner: blockingSTT, mu: &mu, count: &recognizeCalls}

	vad := &eagerFakeVAD{
		responses: []struct {
			segment []byte
			isFinal bool
		}{
			{[]byte("s1"), false},
			{[]byte("s2"), false},
			{[]byte("s3"), false},
		},
	}
	rec := stt.NewEagerRecognizer(countingSTT, vad)

	ctx, cancel := context.WithCancel(context.Background())
	input := audio.NewPipe[audio.Frame](4)
	fmt := audio.Format{Codec: audio.CodecPCM, SampleRate: 16000}

	input.Send(audio.Frame{Data: []byte("a"), Format: fmt})
	input.Send(audio.Frame{Data: []byte("b"), Format: fmt})
	input.Send(audio.Frame{Data: []byte("c"), Format: fmt})
	input.Close()

	stream, err := rec.RecognizeStream(ctx, input)
	if err != nil {
		t.Fatalf("RecognizeStream: %v", err)
	}

	// Read first result, then cancel.
	_, err = stream.Read()
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}

	cancel()

	// Drain remaining — should terminate with context.Canceled or EOF.
	for {
		_, err := stream.Read()
		if err != nil {
			break
		}
	}
}

// countingSTTWrapper wraps an STT and counts Recognize calls.
type countingSTTWrapper struct {
	inner stt.STT
	mu    *sync.Mutex
	count *int
}

func (w *countingSTTWrapper) Recognize(ctx context.Context, input audio.Frame, opts ...stt.STTOption) (stt.STTResult, error) {
	w.mu.Lock()
	*w.count++
	w.mu.Unlock()
	return w.inner.Recognize(ctx, input, opts...)
}

func TestEagerRecognizer_EmptyStream(t *testing.T) {
	sttImpl := &eagerFakeSTT{results: []stt.STTResult{{Text: "flush", IsFinal: true}}}
	vad := &eagerFakeVAD{flush: []byte("flushed-audio")}

	rec := stt.NewEagerRecognizer(sttImpl, vad)
	input := audio.NewPipe[audio.Frame](4)
	input.Close() // close immediately; Flush provides data

	ctx := context.Background()
	stream, err := rec.RecognizeStream(ctx, input)
	if err != nil {
		t.Fatalf("RecognizeStream: %v", err)
	}

	var results []stt.STTResult
	for {
		r, err := stream.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		results = append(results, r)
	}

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Text != "flush" {
		t.Errorf("Text = %q, want flush", results[0].Text)
	}
	if !bytes.Equal(results[0].Audio.Data, []byte("flushed-audio")) {
		t.Errorf("Audio.Data = %q, want flushed-audio", results[0].Audio.Data)
	}
}

func TestEagerRecognizer_EmptyStreamNoFlush(t *testing.T) {
	sttImpl := &eagerFakeSTT{results: nil}
	vad := &eagerFakeVAD{flush: nil} // no flush data

	rec := stt.NewEagerRecognizer(sttImpl, vad)
	input := audio.NewPipe[audio.Frame](4)
	input.Close()

	ctx := context.Background()
	stream, err := rec.RecognizeStream(ctx, input)
	if err != nil {
		t.Fatalf("RecognizeStream: %v", err)
	}

	var results []stt.STTResult
	for {
		r, err := stream.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		results = append(results, r)
	}

	if len(results) != 0 {
		t.Errorf("got %d results, want 0 (no flush data)", len(results))
	}
}

func TestEagerRecognizer_ResultAudio(t *testing.T) {
	audioData := []byte("original-audio")
	sttImpl := &eagerFakeSTT{results: []stt.STTResult{{Text: "ok", IsFinal: true}}}
	vad := &eagerFakeVAD{
		responses: []struct {
			segment []byte
			isFinal bool
		}{
			{append([]byte{}, audioData...), true},
		},
	}

	rec := stt.NewEagerRecognizer(sttImpl, vad)
	input := audio.NewPipe[audio.Frame](4)
	fmt := audio.Format{Codec: audio.CodecPCM, SampleRate: 16000}
	input.Send(audio.Frame{Data: audioData, Format: fmt})
	input.Close()

	ctx := context.Background()
	stream, err := rec.RecognizeStream(ctx, input)
	if err != nil {
		t.Fatalf("RecognizeStream: %v", err)
	}

	r, err := stream.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// Consume until EOF
	for {
		_, err := stream.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}

	// Result.Audio must be an independent copy (Frame with Data not sharing backing with input)
	if !bytes.Equal(r.Audio.Data, []byte("original-audio")) {
		t.Errorf("Audio.Data = %q, want original-audio", r.Audio.Data)
	}
	// Mutate original and ensure Result.Audio is unchanged
	audioData[0] = 'X'
	if bytes.Equal(r.Audio.Data, audioData) {
		t.Error("Result.Audio.Data appears to share backing with input; want independent copy")
	}
}
