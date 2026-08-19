package inference_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func TestAssemblyTranscribeStreamReturnsTranscript(t *testing.T) {
	assembly := (&inferencetest.TranscriptionFake{}).Assembly(t)
	format := media.AudioFormat{
		Encoding:     media.AudioEncodingPCM16,
		SampleRateHz: 16000,
		Channels:     1,
	}
	source, err := media.NewAudioBytes([]byte{0, 0}, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	pipe := message.NewPartPipe(1)
	pipe.Send(message.AudioPart{Source: source, Format: &format})
	pipe.Close()
	response, err := assembly.TranscribeStream(
		context.Background(),
		inferencetest.DefaultFakeTranscribeModel,
		inference.TranscriptionSessionRequest{InputFormat: format},
		pipe,
	)
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	if response.Text != "ok" {
		t.Fatalf("Text = %q, want %q", response.Text, "ok")
	}
}

func TestAssemblyTranscribeStreamRejectsBadItems(t *testing.T) {
	assembly := (&inferencetest.TranscriptionFake{}).Assembly(t)
	format := media.AudioFormat{
		Encoding:     media.AudioEncodingPCM16,
		SampleRateHz: 16000,
		Channels:     1,
	}
	pipe := message.NewPartPipe(1)
	pipe.Send(message.TextPart{Text: "nope"})
	pipe.Close()
	_, err := assembly.TranscribeStream(
		context.Background(),
		inferencetest.DefaultFakeTranscribeModel,
		inference.TranscriptionSessionRequest{InputFormat: format},
		pipe,
	)
	if err == nil || !strings.Contains(err.Error(), "want audio part") {
		t.Fatalf("TranscribeStream = %v, want audio-part error", err)
	}
}

func TestTranscriptionRequestRejectsStreamSource(t *testing.T) {
	pipe := message.NewPartPipe(0)
	source, err := message.NewAudioStream(pipe, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioStream: %v", err)
	}
	request := inference.TranscriptionRequest{Audio: source}
	err = request.Validate()
	if err == nil || !strings.Contains(err.Error(), "TranscribeSession") {
		t.Fatalf("Validate = %v, want TranscribeSession rejection", err)
	}
}
