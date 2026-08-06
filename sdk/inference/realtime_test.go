package inference

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

func TestRealtimeContractsOwnMutableInputsAndValidateModalities(t *testing.T) {
	config := RealtimeConfig{
		Modalities: []Modality{ModalityText, ModalityAudio},
		InputAudioFormat: &media.AudioFormat{
			Encoding:     media.AudioEncodingPCM16,
			SampleRateHz: 24_000,
			Channels:     1,
		},
		OutputAudioFormat: &media.AudioFormat{
			Encoding:     media.AudioEncodingPCM16,
			SampleRateHz: 24_000,
			Channels:     1,
		},
		Voice: &media.VoiceSpec{ID: "alloy"},
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("config Validate: %v", err)
	}
	config.Modalities = append(config.Modalities, ModalityAudio)
	if err := config.Validate(); err == nil {
		t.Fatal("duplicate realtime modality was accepted")
	}
	config.Modalities = []Modality{ModalityVideo}
	if err := config.Validate(); err == nil {
		t.Fatal("unsupported realtime video output was accepted")
	}
	config.Modalities = []Modality{ModalityText}
	if err := config.Validate(); err == nil {
		t.Fatal("audio output settings without audio modality were accepted")
	}
	duplicateTools := RealtimeConfig{
		Modalities: []Modality{ModalityText},
		Tools: []message.Definition{
			{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	if err := duplicateTools.Validate(); err == nil {
		t.Fatal("duplicate realtime tools were accepted")
	}

	input := RealtimeAudioInput{Chunk: media.AudioChunk{Data: []byte("pcm")}}
	clone := input.Clone().(RealtimeAudioInput)
	clone.Chunk.Data[0] = 'X'
	if string(input.Chunk.Data) != "pcm" {
		t.Fatal("RealtimeAudioInput.Clone shared audio bytes")
	}
	var _ RealtimeSession = (*fakeRealtimeSession)(nil)
}

func TestRealtimeEventsCloneMutablePayloads(t *testing.T) {
	event := RealtimeAudioDeltaEvent{
		Chunk: media.AudioChunk{Data: []byte("audio")},
	}
	clone := event.Clone().(RealtimeAudioDeltaEvent)
	event.Chunk.Data[0] = 'X'
	if string(clone.Chunk.Data) != "audio" {
		t.Fatal("realtime audio event clone aliases provider bytes")
	}
}

func TestRealtimeConfigClonePreservesAndOwnsTools(t *testing.T) {
	config := RealtimeConfig{
		Modalities: []Modality{ModalityText},
		Tools: []message.Definition{{
			Name:        "search",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
	clone := config.Clone()
	if len(clone.Tools) != 1 || clone.Tools[0].Name != "search" {
		t.Fatalf("clone tools = %+v", clone.Tools)
	}
	clone.Tools[0].InputSchema[0] = '['
	if string(config.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatal("RealtimeConfig.Clone shared tool schema bytes")
	}
}

type fakeRealtimeSession struct{}

func (*fakeRealtimeSession) Send(context.Context, RealtimeInput) error { return nil }

func (*fakeRealtimeSession) Next(context.Context) (RealtimeEvent, error) {
	return nil, io.EOF
}

func (*fakeRealtimeSession) CancelResponse(context.Context) error { return nil }

func (*fakeRealtimeSession) Close() error { return nil }
