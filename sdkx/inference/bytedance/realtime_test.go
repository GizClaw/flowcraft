package bytedance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// realtimeConfig is the minimal valid duplex dialogue config.
func realtimeConfig() inference.RealtimeConfig {
	return inference.RealtimeConfig{
		Modalities: []inference.Modality{
			inference.ModalityAudio,
			inference.ModalityText,
		},
	}
}

func compileRealtimeForTest(
	config inference.RealtimeConfig,
) (inference.Compiled[realtimeWire], error) {
	return compileRealtime(&clients{})(
		context.Background(),
		generateModel("doubao-seeduplex-3-0"),
		config,
	)
}

func TestRealtimeCompileAcceptance(t *testing.T) {
	config := realtimeConfig()
	config.Instructions = "speak slowly"
	config.InputAudioFormat = &media.AudioFormat{
		Encoding:     media.AudioEncodingPCM16,
		SampleRateHz: 16000,
		Channels:     1,
	}
	config.OutputAudioFormat = &media.AudioFormat{
		Encoding:     media.AudioEncodingPCM16,
		SampleRateHz: 24000,
		Channels:     1,
	}
	config.Voice = &media.VoiceSpec{ID: "zh_female_cancan"}
	config.Tools = []message.Definition{{
		Name:        "lookup",
		Description: "look things up",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"q": {"type": "string", "maxLength": 64}},
			"required": ["q"]
		}`),
	}}
	speed, loudness := 10, -5
	config.Extensions = inference.Extensions{
		RealtimeOptions{OutputSpeed: &speed, OutputLoudness: &loudness},
	}

	compiled, err := compileRealtimeForTest(config)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := compiled.Wire
	if !wire.audioOutput || !wire.textOutput {
		t.Fatalf("modalities = %+v", wire)
	}
	if wire.instructions != "speak slowly" {
		t.Fatalf("instructions = %q", wire.instructions)
	}
	if wire.inputFormat.rate != 16000 || wire.outputFormat.rate != 24000 {
		t.Fatalf("formats = %+v / %+v", wire.inputFormat, wire.outputFormat)
	}
	if wire.voice != "zh_female_cancan" {
		t.Fatalf("voice = %q", wire.voice)
	}
	if len(wire.tools) != 1 || wire.tools[0].name != "lookup" {
		t.Fatalf("tools = %+v", wire.tools)
	}
	schema := wire.tools[0].schema
	if schema == nil || schema.Type != "object" || schema.Properties["q"] == nil {
		t.Fatalf("schema = %+v", schema)
	}
	if schema.Properties["q"].MaxLength == nil || *schema.Properties["q"].MaxLength != 64 {
		t.Fatalf("maxLength = %+v", schema.Properties["q"])
	}
	if wire.outputSpeed == nil || *wire.outputSpeed != 10 {
		t.Fatalf("speed = %+v", wire.outputSpeed)
	}
	if wire.outputLoudness == nil || *wire.outputLoudness != -5 {
		t.Fatalf("loudness = %+v", wire.outputLoudness)
	}
}

func TestRealtimeCompileRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*inference.RealtimeConfig)
		field  inference.FieldID
		kind   inference.ErrorKind
	}{
		{
			name: "text-only output has no native mode",
			mutate: func(c *inference.RealtimeConfig) {
				c.Modalities = []inference.Modality{inference.ModalityText}
			},
			field: inference.FieldRealtimeModalities,
			kind:  inference.UnsupportedFeature,
		},
		{
			name: "compressed input format",
			mutate: func(c *inference.RealtimeConfig) {
				c.InputAudioFormat = &media.AudioFormat{
					Encoding: media.AudioEncodingOpus,
				}
			},
			field: inference.FieldRealtimeInputAudioFormat,
			kind:  inference.UnsupportedFeature,
		},
		{
			name: "rate outside published set",
			mutate: func(c *inference.RealtimeConfig) {
				c.OutputAudioFormat = &media.AudioFormat{
					Encoding:     media.AudioEncodingPCM16,
					SampleRateHz: 44100,
					Channels:     1,
				}
			},
			field: inference.FieldRealtimeOutputAudioFormat,
			kind:  inference.UnsupportedFeature,
		},
		{
			name: "voice language is fixed per voice",
			mutate: func(c *inference.RealtimeConfig) {
				c.Voice = &media.VoiceSpec{ID: "zh_female_cancan", Language: "en-US"}
			},
			field: inference.FieldRealtimeVoice,
			kind:  inference.UnsupportedFeature,
		},
		{
			name: "schema keyword outside subset",
			mutate: func(c *inference.RealtimeConfig) {
				c.Tools = []message.Definition{{
					Name:        "lookup",
					InputSchema: json.RawMessage(`{"type":"object","patternProperties":{"^x":{}}}`),
				}}
			},
			field: inference.FieldRealtimeTools,
			kind:  inference.UnsupportedFeature,
		},
		{
			name: "foreign operation extension",
			mutate: func(c *inference.RealtimeConfig) {
				off := false
				c.Extensions = inference.Extensions{
					TTSOptions{PitchRate: new(int)},
					TranscriptionOptions{ITN: &off},
				}
			},
			field: "extension.bytedance.tts_options.pitch_rate",
			kind:  inference.InvalidExtension,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := realtimeConfig()
			tc.mutate(&config)
			_, err := compileRealtimeForTest(config)
			if err == nil {
				t.Fatal("expected compiler rejection")
			}
			if !inference.IsKind(err, tc.kind) {
				t.Fatalf("kind = %v", err)
			}
			var inferenceErr *inference.Error
			if !errors.As(err, &inferenceErr) || inferenceErr.Field != tc.field {
				t.Fatalf("field = %v, want %s", err, tc.field)
			}
		})
	}
}

func TestRealtimeInputCompile(t *testing.T) {
	compiled, err := compileRealtimeInput(
		context.Background(),
		generateModel("doubao-seeduplex-3-0"),
		inference.RealtimeTextInput{Text: "hello"},
	)
	if err != nil {
		t.Fatalf("compile text: %v", err)
	}
	if compiled.Wire.kind != "text" || compiled.Wire.text != "hello" {
		t.Fatalf("wire = %+v", compiled.Wire)
	}

	compiled, err = compileRealtimeInput(
		context.Background(),
		generateModel("doubao-seeduplex-3-0"),
		inference.RealtimeToolResultInput{Result: message.Result{
			CallID:  "call_1",
			Content: "done",
		}},
	)
	if err != nil {
		t.Fatalf("compile tool result: %v", err)
	}
	if compiled.Wire.kind != "tool_result" || compiled.Wire.callID != "call_1" {
		t.Fatalf("wire = %+v", compiled.Wire)
	}

	frame, err := media.NewImageBytes([]byte{0xff, 0xd8, 0xff}, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	_, err = compileRealtimeInput(
		context.Background(),
		generateModel("doubao-seeduplex-3-0"),
		inference.RealtimeVideoInput{Frame: media.VideoFrame{Source: frame}},
	)
	if err == nil {
		t.Fatal("expected video input rejection")
	}
	var inferenceErr *inference.Error
	if !errors.As(err, &inferenceErr) || inferenceErr.Field != inference.FieldRealtimeInputVideo {
		t.Fatalf("field = %v", err)
	}
}
