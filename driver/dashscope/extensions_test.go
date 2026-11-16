package dashscope

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
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

// Clone is what the runtime hands a second caller: a clone must not share storage
// with the original, or one request's tuning would leak into another's.
func TestExtensionCloneIsDeep(t *testing.T) {
	generate := GenerateOptions{
		Temperature: extPtr(1.0),
		Stop:        []string{"\n\n"},
		ResponseFormat: &ResponseFormat{
			Kind: "json_schema", Name: "out", Schema: []byte(`{"type":"object"}`),
		},
	}
	clonedGenerate := generate.Clone().(GenerateOptions)
	*clonedGenerate.Temperature = 0.1
	clonedGenerate.Stop[0] = "END"
	clonedGenerate.ResponseFormat.Name = "other"
	clonedGenerate.ResponseFormat.Schema[0] = '['
	if *generate.Temperature != 1.0 {
		t.Errorf("temperature mutated through the clone: %v", *generate.Temperature)
	}
	if generate.Stop[0] != "\n\n" {
		t.Errorf("stop mutated through the clone: %v", generate.Stop)
	}
	if generate.ResponseFormat.Name != "out" {
		t.Errorf("response format mutated through the clone: %v", generate.ResponseFormat.Name)
	}
	if string(generate.ResponseFormat.Schema) != `{"type":"object"}` {
		t.Errorf("schema mutated through the clone: %s", generate.ResponseFormat.Schema)
	}

	image := ImageOptions{Size: &media.ImageSize{Width: 1024, Height: 1024}, Count: extPtr(2)}
	clonedImage := image.Clone().(ImageOptions)
	clonedImage.Size.Width = 512
	*clonedImage.Count = 1
	if image.Size.Width != 1024 || *image.Count != 2 {
		t.Errorf("image options mutated through the clone: %+v", image)
	}

	video := VideoOptions{DurationMillis: extPtr(int64(5000)), Watermark: extPtr(false)}
	clonedVideo := video.Clone().(VideoOptions)
	*clonedVideo.DurationMillis = 1
	*clonedVideo.Watermark = true
	if *video.DurationMillis != 5000 || *video.Watermark {
		t.Errorf("video options mutated through the clone: %+v", video)
	}

	// The vocabulary is a map: a shallow copy would let one transcription's terms
	// reach another's.
	transcribe := TranscribeOptions{
		ChannelID:  extPtr(1),
		Vocabulary: map[string]int{"flow": 50, "craft": 3},
	}
	clonedTranscribe := transcribe.Clone().(TranscribeOptions)
	*clonedTranscribe.ChannelID = 2
	clonedTranscribe.Vocabulary["flow"] = 1
	delete(clonedTranscribe.Vocabulary, "craft")
	if *transcribe.ChannelID != 1 {
		t.Errorf("channel id mutated through the clone: %v", *transcribe.ChannelID)
	}
	if transcribe.Vocabulary["flow"] != 50 || transcribe.Vocabulary["craft"] != 3 {
		t.Errorf("vocabulary mutated through the clone: %v", transcribe.Vocabulary)
	}

	// The types that hold no storage clone to themselves; asserting the id keeps
	// the table honest if one grows a field later.
	for _, extension := range []inference.Extension{
		EmbedOptions{TextType: "query"},
		TTSOptions{Voice: "Cherry"},
	} {
		cloned := extension.Clone()
		if cloned.ExtensionID() != extension.ExtensionID() {
			t.Errorf("%T clone has id %q, want %q",
				extension, cloned.ExtensionID(), extension.ExtensionID())
		}
	}
}

// ActiveFields is what the runtime uses to report which declarations a request
// actually used, so a field present in the document but not set must not be
// reported.
func TestExtensionActiveFields(t *testing.T) {
	options := GenerateOptions{Temperature: extPtr(0.5), Stop: []string{"x"}}
	fields := options.ActiveFields()
	seen := map[inference.ExtensionField]bool{}
	for _, field := range fields {
		seen[field] = true
	}
	if !seen["temperature"] || !seen["stop"] {
		t.Errorf("active fields = %v, want temperature and stop", fields)
	}
	if seen["max_output_tokens"] {
		t.Errorf("unset field reported: %v", fields)
	}

	tts := TTSOptions{Voice: "Cherry"}
	if fields := tts.ActiveFields(); len(fields) != 1 || fields[0] != "voice" {
		t.Errorf("tts active fields = %v, want voice", fields)
	}
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
	if (GenerateOptions{}).ExtensionID() != extensionGenerate {
		t.Errorf("generate id = %q", (GenerateOptions{}).ExtensionID())
	}
}
