package dashscope

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func extPtr[T any](value T) *T { return &value }

// Each extension declares what the platform accepts, and Validate is the gate a
// deployment's document goes through before a request is built. The refusals are
// what a misconfigured deployment sees, so each rule gets a case.
func TestExtensionValidation(t *testing.T) {
	cases := []struct {
		name      string
		extension inference.Extension
		want      string
	}{
		{"temperature high", GenerateOptions{Temperature: extPtr(2.5)}, "temperature must be within [0, 2]"},
		{"temperature negative", GenerateOptions{Temperature: extPtr(-0.1)},
			"temperature must be within [0, 2]"},
		{"top p", GenerateOptions{TopP: extPtr(1.5)}, "top_p must be within [0, 1]"},
		{"max tokens", GenerateOptions{MaxOutputTokens: extPtr(0)}, "max_output_tokens must be positive"},
		{"top k", GenerateOptions{TopK: extPtr(0)}, "top_k must be positive"},
		{"thinking budget", GenerateOptions{ThinkingBudget: extPtr(-1)}, "thinking_budget must not be negative"},
		{"response kind", GenerateOptions{ResponseFormat: &ResponseFormat{Kind: "xml"}},
			`unknown response format "xml"`},
		{"schema without name", GenerateOptions{ResponseFormat: &ResponseFormat{
			Kind: "json_schema", Schema: []byte(`{"type":"object"}`),
		}}, "json_schema response needs a name and schema"},
		{"schema without body", GenerateOptions{ResponseFormat: &ResponseFormat{
			Kind: "json_schema", Name: "out", Schema: nil,
		}}, "json_schema response needs a name and schema"},
		{"schema not json", GenerateOptions{ResponseFormat: &ResponseFormat{
			Kind: "json_schema", Name: "out", Schema: []byte(`{not json`),
		}}, "json_schema response schema is invalid"},
		{"text type", EmbedOptions{TextType: "paragraph"}, `unknown text_type "paragraph"`},
		{"image size", ImageOptions{Size: &media.ImageSize{Width: 0, Height: 10}},
			"image dimensions must be positive"},
		{"image count", ImageOptions{Count: extPtr(0)}, "count must be positive"},
		{"image format", ImageOptions{OutputFormat: media.ImageFormatGIF},
			`unsupported image format "gif"`},
		{"image negative prompt", ImageOptions{NegativePrompt: strings.Repeat("x", 501)},
			"negative_prompt must be at most 500 characters"},
		{"image ref strength", ImageOptions{RefStrength: extPtr(1.5)}, "ref_strength must be between 0 and 1"},
		{"image ref mode", ImageOptions{RefMode: "copy"}, `unknown ref_mode "copy"`},
		{"image upscale", ImageOptions{UpscaleFactor: extPtr(0.0)}, "upscale_factor must be positive"},
		{"video duration", VideoOptions{DurationMillis: extPtr(int64(0))}, "duration_millis must be positive"},
		{"video negative prompt", VideoOptions{NegativePrompt: strings.Repeat("x", 501)},
			"negative_prompt must be at most 500 characters"},
		{"video link", VideoOptions{Link: "ftp://example.test/x.mp4"}, "link must be an http(s) URL"},
		{"video shot type", VideoOptions{ShotType: "panorama"}, `unknown shot_type "panorama"`},
		{"transcribe channel", TranscribeOptions{ChannelID: extPtr(-1)}, "channel_id must not be negative"},
		{"transcribe vocabulary", TranscribeOptions{Vocabulary: map[string]int{"flow": 9}},
			`vocabulary weight for "flow" must be 1-5 or 50`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := test.extension.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", test.name)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to name %q", err, test.want)
			}
		})
	}

	// The same declarations, sane: a document that a deployment could write has
	// to pass, or the test above would only prove the gate is always shut.
	valid := []inference.Extension{
		GenerateOptions{
			Temperature: extPtr(1.0), TopP: extPtr(0.9), MaxOutputTokens: extPtr(1024),
			TopK: extPtr(10), ThinkingBudget: extPtr(0),
			ResponseFormat: &ResponseFormat{Kind: "json_object"},
		},
		GenerateOptions{ResponseFormat: &ResponseFormat{
			Kind: "json_schema", Name: "out", Schema: []byte(`{"type":"object"}`),
		}},
		EmbedOptions{TextType: "query"},
		TTSOptions{Voice: "Cherry"},
		ImageOptions{OutputFormat: media.ImageFormatPNG, RefMode: "repaint", RefStrength: extPtr(0.5)},
		VideoOptions{ShotType: "multi", Link: "https://example.test/x.mp4"},
		TranscribeOptions{ChannelID: extPtr(0), Vocabulary: map[string]int{"flow": 50}},
	}
	for _, extension := range valid {
		if err := extension.Validate(); err != nil {
			t.Errorf("%T %s refused: %v", extension, extension.ExtensionID(), err)
		}
	}
}

// The contract every extension owes — an addressable identity, an ActiveFields
// that reports exactly the options it sets, a Validate that accepts its own
// sample, and a Clone that owns its storage — is verified by the shared suite
// rather than re-derived here. What stays in this file is this driver's own
// business: which declarations the platform refuses.
func TestExtensionContract(t *testing.T) {
	generate := GenerateOptions{
		Temperature:            extPtr(1.0),
		TopP:                   extPtr(0.9),
		MaxOutputTokens:        extPtr(1024),
		Seed:                   extPtr(int64(7)),
		TopK:                   extPtr(10),
		RepetitionPenalty:      extPtr(1.05),
		PresencePenalty:        extPtr(0.1),
		Stop:                   []string{"\n\n"},
		ParallelToolCalls:      extPtr(true),
		EnableSearch:           extPtr(true),
		ThinkingBudget:         extPtr(512),
		VlHighResolutionImages: extPtr(true),
		ResponseFormat: &ResponseFormat{
			Kind: "json_schema", Name: "out", Schema: []byte(`{"type":"object"}`), Strict: true,
		},
	}
	tts := TTSOptions{Voice: "Cherry", Language: "zh"}
	image := ImageOptions{
		Size:             &media.ImageSize{Width: 1024, Height: 1024},
		Count:            extPtr(2),
		Seed:             extPtr(int64(3)),
		OutputFormat:     media.ImageFormatPNG,
		NegativePrompt:   "blurry",
		PromptExtend:     extPtr(true),
		Watermark:        extPtr(false),
		Style:            "photographic",
		RefStrength:      extPtr(0.5),
		RefMode:          "repaint",
		Function:         "text_to_image",
		IsSketch:         extPtr(false),
		UpscaleFactor:    extPtr(2.0),
		EnableInterleave: extPtr(true),
	}
	video := VideoOptions{
		DurationMillis: extPtr(int64(5000)),
		Resolution:     "1080P",
		AspectRatio:    "16:9",
		Seed:           extPtr(int64(9)),
		Watermark:      extPtr(true),
		NegativePrompt: "shake",
		PromptExtend:   extPtr(false),
		ShotType:       "multi",
		Audio:          extPtr(true),
		Template:       "news",
		Link:           "https://example.test/clip.mp4",
		SmartDuration:  true,
	}
	transcribe := TranscribeOptions{
		ChannelID:  extPtr(1),
		Vocabulary: map[string]int{"flow": 50, "craft": 3},
	}
	inferencetest.RunExtensions(t,
		inferencetest.ExtensionSample{
			Name:  generate.ExtensionID(),
			Value: &generate,
			Fields: []inference.ExtensionField{
				"temperature", "top_p", "max_output_tokens", "seed", "top_k",
				"repetition_penalty", "presence_penalty", "stop",
				"parallel_tool_calls", "enable_search", "thinking_budget",
				"vl_high_resolution_images", "response_format",
			},
		},
		inferencetest.ExtensionSample{
			Name:   EmbedOptions{}.ExtensionID(),
			Value:  &EmbedOptions{TextType: "query"},
			Fields: []inference.ExtensionField{"text_type"},
		},
		inferencetest.ExtensionSample{
			Name:   tts.ExtensionID(),
			Value:  &tts,
			Fields: []inference.ExtensionField{"voice", "language"},
		},
		inferencetest.ExtensionSample{
			Name:  image.ExtensionID(),
			Value: &image,
			Fields: []inference.ExtensionField{
				"size", "count", "seed", "output_format", "negative_prompt",
				"prompt_extend", "watermark", "style", "ref_strength", "ref_mode",
				"function", "is_sketch", "upscale_factor", "enable_interleave",
			},
		},
		inferencetest.ExtensionSample{
			Name:  video.ExtensionID(),
			Value: &video,
			Fields: []inference.ExtensionField{
				"duration_millis", "resolution", "aspect_ratio", "seed", "watermark",
				"negative_prompt", "prompt_extend", "shot_type", "audio", "template",
				"link", "smart_duration",
			},
		},
		inferencetest.ExtensionSample{
			Name:   transcribe.ExtensionID(),
			Value:  &transcribe,
			Fields: []inference.ExtensionField{"channel_id", "vocabulary"},
		},
	)
}

// ProviderID namespaces an extension's document key, so a provider without one
// falls back to this driver's.
func TestExtensionProviderID(t *testing.T) {
	if got := (GenerateOptions{}).ProviderID(); got != ProviderID {
		t.Errorf("provider = %q, want %q", got, ProviderID)
	}
	custom := GenerateOptions{Provider: "custom"}
	if got := custom.ProviderID(); got != "custom" {
		t.Errorf("provider = %q, want custom", got)
	}
}

func TestAClonedExtensionIsStillUnderstood(t *testing.T) {
	cloned := inference.Extensions{&GenerateOptions{Temperature: extPtr(0.3)}}.Clone()
	options, err := generateOptions(cloned)
	if err != nil {
		t.Fatalf("a cloned extension was not understood: %v", err)
	}
	if options.Temperature == nil || *options.Temperature != 0.3 {
		t.Errorf("temperature = %v, want the clone's 0.3", options.Temperature)
	}
}
