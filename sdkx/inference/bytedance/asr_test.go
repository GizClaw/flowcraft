package bytedance

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/media"
)

// asrConfig is the minimal valid streaming recognition config.
func asrConfig() inference.TranscriptionSessionConfig {
	return inference.TranscriptionSessionConfig{
		InputFormat: media.AudioFormat{
			Encoding:     media.AudioEncodingPCM16,
			SampleRateHz: 16000,
			Channels:     1,
		},
	}
}

func compileASRForTest(
	config inference.TranscriptionSessionConfig,
) (inference.Compiled[asrWire], error) {
	return compileASR(Spec{})(
		context.Background(),
		generateModel("doubao-asr-sauc-2-0"),
		config,
	)
}

func TestASRCompileAcceptance(t *testing.T) {
	config := asrConfig()
	config.Language = "zh-CN"

	compiled, err := compileASRForTest(config)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := compiled.Wire
	if wire.format != "pcm" || wire.sampleRate != 16000 || wire.channels != 1 {
		t.Fatalf("format = %+v", wire)
	}
	if wire.bits != 16 {
		t.Fatalf("bits = %d", wire.bits)
	}
	if wire.language != "zh-CN" {
		t.Fatalf("language = %q", wire.language)
	}
	if !wire.itn || !wire.punc {
		t.Fatalf("itn/punc default = %v/%v", wire.itn, wire.punc)
	}
	if !wire.timestamps {
		t.Fatal("timestamps default to on")
	}
	if wire.resourceID != "doubao-asr-sauc-2-0" {
		t.Fatalf("resource = %q", wire.resourceID)
	}
}

func TestASRCompileOptions(t *testing.T) {
	disable := false
	config := asrConfig()
	config.Extensions = inference.Extensions{
		TranscriptionOptions{
			Diarization: &disable,
			Hotwords:    []string{"火山引擎", "豆包"},
			ResultType:  "full",
			ITN:         &disable,
		},
	}

	compiled, err := compileASRForTest(config)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := compiled.Wire
	if wire.diarization {
		t.Fatal("diarization should stay disabled")
	}
	if len(wire.hotwords) != 2 || wire.hotwords[0] != "火山引擎" {
		t.Fatalf("hotwords = %v", wire.hotwords)
	}
	if wire.resultType != "full" {
		t.Fatalf("result_type = %q", wire.resultType)
	}
	if wire.itn || !wire.punc {
		t.Fatalf("itn/punc = %v/%v", wire.itn, wire.punc)
	}
}

func TestASRCompileSpeakerNumImpliesDiarization(t *testing.T) {
	speakers := 2
	config := asrConfig()
	config.Extensions = inference.Extensions{
		TranscriptionOptions{SpeakerNum: &speakers},
	}

	compiled, err := compileASRForTest(config)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !compiled.Wire.diarization || compiled.Wire.speakerNum != 2 {
		t.Fatalf("diarization/speaker_num = %+v", compiled.Wire)
	}
}

func TestASRCompileRejections(t *testing.T) {
	disable := false
	cases := []struct {
		name   string
		mutate func(*inference.TranscriptionSessionConfig)
		field  inference.FieldID
		kind   inference.ErrorKind
	}{
		{
			name: "compressed input has no token",
			mutate: func(c *inference.TranscriptionSessionConfig) {
				c.InputFormat.Encoding = media.AudioEncodingMP3
			},
			field: inference.FieldTranscriptionInputFormat,
			kind:  inference.UnsupportedFeature,
		},
		{
			name: "sample rate outside published set",
			mutate: func(c *inference.TranscriptionSessionConfig) {
				c.InputFormat.SampleRateHz = 24000
			},
			field: inference.FieldTranscriptionInputFormat,
			kind:  inference.UnsupportedFeature,
		},
		{
			name: "channel count outside mono/stereo",
			mutate: func(c *inference.TranscriptionSessionConfig) {
				c.InputFormat.Channels = 4
			},
			field: inference.FieldTranscriptionInputFormat,
			kind:  inference.UnsupportedFeature,
		},
		{
			name: "language outside published set",
			mutate: func(c *inference.TranscriptionSessionConfig) {
				c.Language = "fr-FR"
			},
			field: inference.FieldTranscriptionLanguage,
			kind:  inference.UnsupportedFeature,
		},
		{
			name: "prompt has no channel",
			mutate: func(c *inference.TranscriptionSessionConfig) {
				c.Prompt = "expect domain terms"
			},
			field: inference.FieldTranscriptionPrompt,
			kind:  inference.UnsupportedFeature,
		},
		{
			name: "speaker hint contradicts disabled diarization",
			mutate: func(c *inference.TranscriptionSessionConfig) {
				speakers := 2
				c.Extensions = inference.Extensions{
					TranscriptionOptions{Diarization: &disable, SpeakerNum: &speakers},
				}
			},
			field: "extension.bytedance.transcription_options.speaker_num",
			kind:  inference.InvalidExtension,
		},
		{
			name: "foreign operation extension",
			mutate: func(c *inference.TranscriptionSessionConfig) {
				voiceless := true
				c.Extensions = inference.Extensions{
					ImageOptions{Watermark: &voiceless},
				}
			},
			field: "extension.bytedance.image_options.watermark",
			kind:  inference.InvalidExtension,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := asrConfig()
			tc.mutate(&config)
			compiled, err := compileASRForTest(config)
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
			if compiled.Wire.resourceID != "" {
				t.Fatal("rejected compile must not produce a wire")
			}
		})
	}
}
