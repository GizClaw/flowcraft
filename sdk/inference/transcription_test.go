package inference

import (
	"context"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message/media"
)

func TestTranscriptionContractsSupportBatchAndDuplexSessions(t *testing.T) {
	source, _ := media.NewAudioBytes([]byte("audio"), "audio/wav")
	timestamps := true
	request := TranscriptionRequest{
		Audio:      source,
		Language:   "en",
		Timestamps: &timestamps,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("request Validate: %v", err)
	}
	response := TranscriptionResponse{
		Text: "hello",
		Language: &TranscriptLanguage{
			Code: "en", Source: TranscriptLanguageDetected, Confidence: scorePointer(0.98),
		},
		Segments: []TranscriptSegment{{
			Text: "hello", Speaker: "A", StartMillis: 0, EndMillis: 500,
			Language: &TranscriptLanguage{
				Code: "en", Source: TranscriptLanguageDetected,
			},
		}},
		Usage: TranscriptionUsage{AudioDurationMillis: 500},
	}
	if err := response.ValidateFor(request); err != nil {
		t.Fatalf("response ValidateFor: %v", err)
	}
	response.Language.Confidence = scorePointer(1.1)
	if err := response.Validate(); err == nil {
		t.Fatal("invalid detected-language confidence was accepted")
	}
	response.Language = &TranscriptLanguage{
		Code: "fr", Source: TranscriptLanguageRequested,
	}
	if err := response.ValidateFor(request); err == nil {
		t.Fatal("mismatched requested-language attribution was accepted")
	}
	config := TranscriptionSessionConfig{
		InputFormat: media.AudioFormat{
			Encoding:     media.AudioEncodingPCM16,
			SampleRateHz: 16_000,
			Channels:     1,
		},
		Language: "en",
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("session config Validate: %v", err)
	}

	var _ TranscriptionSession = (*fakeTranscriptionSession)(nil)
}

func TestTranscriptionEventsCloneMutablePayloads(t *testing.T) {
	confidence := 0.9
	event := FinalTranscriptEvent{
		Text: "hello",
		Language: &TranscriptLanguage{
			Code: "en", Source: TranscriptLanguageDetected, Confidence: &confidence,
		},
		Segments: []TranscriptSegment{{
			Text: "hello",
			Language: &TranscriptLanguage{
				Code: "en", Source: TranscriptLanguageDetected, Confidence: &confidence,
			},
		}},
	}
	clone := event.Clone().(FinalTranscriptEvent)
	*event.Language.Confidence = 0.1
	*event.Segments[0].Language.Confidence = 0.2
	if *clone.Language.Confidence != 0.9 ||
		*clone.Segments[0].Language.Confidence != 0.9 {
		t.Fatal("transcription event clone aliases provider language metadata")
	}
}

func scorePointer(value float64) *float64 { return &value }

type fakeTranscriptionSession struct{}

func (*fakeTranscriptionSession) SendAudio(context.Context, media.AudioChunk) error {
	return nil
}

func (*fakeTranscriptionSession) CloseInput(context.Context) error {
	return nil
}

func (*fakeTranscriptionSession) Next(context.Context) (TranscriptionEvent, error) {
	return nil, io.EOF
}

func (*fakeTranscriptionSession) Result() (TranscriptionResponse, error) {
	return TranscriptionResponse{}, nil
}

func (*fakeTranscriptionSession) Close() error { return nil }
