package inference

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

var feedTestFormat = media.AudioFormat{
	Encoding:     media.AudioEncodingPCM16,
	SampleRateHz: 16000,
	Channels:     1,
}

type recordingFeedSession struct {
	mu        sync.Mutex
	chunks    []media.AudioChunk
	interrupt int
}

func (s *recordingFeedSession) Send(_ context.Context, chunk media.AudioChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = append(s.chunks, chunk.Clone())
	return nil
}

func (s *recordingFeedSession) Next(context.Context) (TranscriptionSessionEvent, error) {
	return TranscriptionSessionEvent{}, io.EOF
}

func (s *recordingFeedSession) Result() (TranscriptionResponse, error) {
	return TranscriptionResponse{Text: "ok"}, nil
}

func (s *recordingFeedSession) Interrupt() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interrupt++
	return nil
}

func (s *recordingFeedSession) Close() error { return nil }

func (s *recordingFeedSession) snapshot() ([]media.AudioChunk, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]media.AudioChunk(nil), s.chunks...), s.interrupt
}

func feedAudioPart(t *testing.T, data string, format *media.AudioFormat) message.Part {
	t.Helper()
	source, err := media.NewAudioBytes([]byte(data), "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	return message.AudioPart{Source: source, Format: format}
}

func TestFeedTranscriptionPumpsAudioPartsWithSequence(t *testing.T) {
	session := &recordingFeedSession{}
	pipe := message.NewPartPipe(3)
	for i, data := range []string{"aa", "bb", "cc"} {
		var format *media.AudioFormat
		if i == 0 {
			format = &feedTestFormat
		}
		pipe.Send(feedAudioPart(t, data, format))
	}
	pipe.Close()
	if err := FeedTranscription(
		context.Background(), session, feedTestFormat, pipe,
	); err != nil {
		t.Fatalf("FeedTranscription: %v", err)
	}
	chunks, interrupts := session.snapshot()
	if interrupts != 0 {
		t.Fatalf("Interrupt calls = %d, want 0", interrupts)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	for i, want := range []string{"aa", "bb", "cc"} {
		if got := string(chunks[i].Data); got != want {
			t.Fatalf("chunk %d data = %q, want %q", i, got, want)
		}
		if chunks[i].Sequence != uint64(i) {
			t.Fatalf("chunk %d sequence = %d, want %d", i, chunks[i].Sequence, i)
		}
	}
}

func TestFeedTranscriptionEOFEndsNormally(t *testing.T) {
	session := &recordingFeedSession{}
	pipe := message.NewPartPipe(0)
	pipe.Close()
	if err := FeedTranscription(
		context.Background(), session, feedTestFormat, pipe,
	); err != nil {
		t.Fatalf("FeedTranscription: %v", err)
	}
	chunks, interrupts := session.snapshot()
	if len(chunks) != 0 || interrupts != 0 {
		t.Fatalf("chunks = %d, interrupts = %d, want 0/0", len(chunks), interrupts)
	}
}

func TestFeedTranscriptionRejectsUnexpectedItems(t *testing.T) {
	session := &recordingFeedSession{}
	pipe := message.NewPartPipe(1)
	pipe.Send(message.TextPart{Text: "not audio"})
	pipe.Close()
	err := FeedTranscription(
		context.Background(), session, feedTestFormat, pipe,
	)
	if err == nil || !strings.Contains(err.Error(), "want audio part") {
		t.Fatalf("FeedTranscription = %v, want audio-part error", err)
	}
	if _, interrupts := session.snapshot(); interrupts != 0 {
		t.Fatal("session was interrupted for an item-shape error")
	}
}

func TestFeedTranscriptionRejectsNonInlineAudio(t *testing.T) {
	session := &recordingFeedSession{}
	source, err := media.NewAudioURL("https://example.com/audio", "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioURL: %v", err)
	}
	pipe := message.NewPartPipe(1)
	pipe.Send(message.AudioPart{Source: source})
	pipe.Close()
	err = FeedTranscription(
		context.Background(), session, feedTestFormat, pipe,
	)
	if err == nil || !strings.Contains(err.Error(), "inline bytes") {
		t.Fatalf("FeedTranscription = %v, want inline-bytes error", err)
	}
}

func TestFeedTranscriptionRejectsFormatMismatch(t *testing.T) {
	session := &recordingFeedSession{}
	other := media.AudioFormat{
		Encoding:     media.AudioEncodingPCM16,
		SampleRateHz: 8000,
		Channels:     1,
	}
	pipe := message.NewPartPipe(1)
	pipe.Send(feedAudioPart(t, "aa", &other))
	pipe.Close()
	err := FeedTranscription(
		context.Background(), session, feedTestFormat, pipe,
	)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("FeedTranscription = %v, want format-mismatch error", err)
	}
}

func TestFeedTranscriptionInterruptsSessionOnStreamFailure(t *testing.T) {
	session := &recordingFeedSession{}
	pipe := message.NewPartPipe(1)
	pipe.Send(feedAudioPart(t, "aa", nil))
	pipe.Interrupt()
	err := FeedTranscription(
		context.Background(), session, feedTestFormat, pipe,
	)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("FeedTranscription = %v, want context.Canceled", err)
	}
	if _, interrupts := session.snapshot(); interrupts != 1 {
		t.Fatalf("Interrupt calls = %d, want 1", interrupts)
	}
}

func TestFeedTranscriptionRejectsNilInputs(t *testing.T) {
	if err := FeedTranscription(
		context.Background(), nil, feedTestFormat, message.NewPartPipe(0),
	); err == nil {
		t.Fatal("FeedTranscription accepted a nil session")
	}
	if err := FeedTranscription(
		context.Background(), &recordingFeedSession{}, feedTestFormat, nil,
	); err == nil {
		t.Fatal("FeedTranscription accepted a nil stream")
	}
}
