package dashscope

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// imageCapabilities and videoCapabilities render the minimal capability sets
// the family contract checks against.
func imageCapabilities() model.ModelCapabilities {
	return model.ModelCapabilities{Outputs: []message.PartKind{message.PartImage}}
}

func videoCapabilities() model.ModelCapabilities {
	return model.ModelCapabilities{Outputs: []message.PartKind{message.PartVideo}}
}

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		value  string
		width  int
		height int
		ok     bool
	}{
		{value: "1024*1024", width: 1024, height: 1024, ok: true},
		{value: "1280*720", width: 1280, height: 720, ok: true},
		{value: "1280x720", width: 1280, height: 720, ok: true},
		{value: "1280×720", width: 1280, height: 720, ok: true},
		{value: " 720 * 1280 ", width: 720, height: 1280, ok: true},
		{value: "1024", ok: false},
		{value: "0*720", ok: false},
		{value: "wide*tall", ok: false},
	} {
		size, err := ParseSize(tc.value)
		if !tc.ok {
			if err == nil {
				t.Errorf("ParseSize(%q) = %+v, want an error", tc.value, size)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q): %v", tc.value, err)
			continue
		}
		if size.Width != tc.width || size.Height != tc.height {
			t.Errorf("ParseSize(%q) = %dx%d", tc.value, size.Width, size.Height)
		}
	}
}

func TestSizeTokenAndNormalizeResolution(t *testing.T) {
	if got := SizeToken(media.ImageSize{Width: 1280, Height: 720}); got != "1280*720" {
		t.Errorf("SizeToken = %q", got)
	}
	if got := NormalizeResolution(" 720p "); got != "720P" {
		t.Errorf("NormalizeResolution = %q", got)
	}
}

func TestModelSpecValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		model  ModelSpec
		reason string
	}{
		{
			name:   "invalid name",
			model:  ModelSpec{Name: "wan x", Kind: "image"},
			reason: "invalid model name",
		},
		{
			name:   "invalid wire model",
			model:  ModelSpec{Name: "wanx-v1", WireModel: "wan x", Kind: "image"},
			reason: "invalid wire_model",
		},
		{
			name:   "sync api on the editing surface is allowed",
			model:  ModelSpec{Name: "wanx2.6-edit", Kind: "image", Image: ImageParams{API: imageAPISync, Edit: true, ReferenceImage: true}},
			reason: "",
		},
		{
			name:   "unknown image api",
			model:  ModelSpec{Name: "wanx-v1", Kind: "image", Image: ImageParams{API: "stream"}},
			reason: "unknown api",
		},
		{
			name:   "size bounds inverted",
			model:  ModelSpec{Name: "wanx-v1", Kind: "image", Image: ImageParams{MinEdge: 2000, MaxEdge: 1440}},
			reason: "exceeds max_edge",
		},
		{
			name:   "pixel bounds inverted",
			model:  ModelSpec{Name: "wanx-v1", Kind: "image", Image: ImageParams{MinPixels: 100, MaxPixels: 10}},
			reason: "exceeds max_pixels",
		},
		{
			name:   "unparsable declared size",
			model:  ModelSpec{Name: "wanx-v1", Kind: "image", Image: ImageParams{Sizes: []string{"big"}}},
			reason: "must use the form",
		},
		{
			name:   "unknown format",
			model:  ModelSpec{Name: "wanx-v1", Kind: "image", Image: ImageParams{Format: "tiff"}},
			reason: "unknown image format",
		},
		{
			name:   "unknown video size param",
			model:  ModelSpec{Name: "wan2.2-t2v-plus", Kind: "video", Video: VideoParams{SizeParam: "megapixels"}},
			reason: "unknown size_param",
		},
		{
			name:   "declared ratio that is not a ratio",
			model:  ModelSpec{Name: "wan2.2-t2v-plus", Kind: "video", Video: VideoParams{Ratios: []string{"wide"}}},
			reason: "aspect ratio must use width:height",
		},
		{
			name:   "negative duration bound",
			model:  ModelSpec{Name: "wan2.2-t2v-plus", Kind: "video", Video: VideoParams{MaxDuration: -1}},
			reason: "must not be negative",
		},
		{
			name:   "video parameters on an image model",
			model:  ModelSpec{Name: "wanx-v1", Kind: "image", Video: VideoParams{Resolutions: []string{"720P"}}},
			reason: "video parameters on kind",
		},
		{
			name:   "tts parameters on a generate model",
			model:  ModelSpec{Name: "qwen3.7-max", Kind: "generate", TTS: TTSParams{PromptLimit: 600}},
			reason: "sets tts parameters on kind",
		},
		{
			name:   "negative speech prompt limit",
			model:  ModelSpec{Name: "qwen3-tts-flash", Kind: "tts", TTS: TTSParams{PromptLimit: -1}},
			reason: "must not be negative",
		},
		{
			name:   "speech encoding outside the canonical alphabet",
			model:  ModelSpec{Name: "qwen3-tts-flash", Kind: "tts", TTS: TTSParams{Encoding: "ogg"}},
			reason: "unknown encoding",
		},
		{
			name: "raw speech encoding without a rate and channel count",
			model: ModelSpec{
				Name: "qwen3-tts-flash",
				Kind: "tts",
				TTS:  TTSParams{Encoding: media.AudioEncodingPCM16},
			},
			reason: "requires sample_rate_hz and channels",
		},
		{
			name:   "unknown generate surface",
			model:  ModelSpec{Name: "qwen3.7-max", Kind: "generate", Generate: GenerateParams{API: "responses"}},
			reason: "unknown api",
		},
		{
			name:   "unknown max tokens parameter",
			model:  ModelSpec{Name: "qwen3.7-max", Kind: "generate", Generate: GenerateParams{MaxTokensParam: "tokens"}},
			reason: "unknown max_tokens_param",
		},
		{
			name:   "generate parameters on a video model",
			model:  ModelSpec{Name: "wan2.2-t2v-plus", Kind: "video", Generate: GenerateParams{ReplayReasoning: true}},
			reason: "generate parameters on kind",
		},
		{
			name:   "embed parameters on a generate model",
			model:  ModelSpec{Name: "qwen3.7-max", Kind: "generate", Embed: EmbedParams{BatchMax: 5}},
			reason: "embed parameters on kind",
		},
		{
			name:   "negative embed batch",
			model:  ModelSpec{Name: "qwen3.7-text-embedding", Kind: "embed", Embed: EmbedParams{BatchMax: -1}},
			reason: "batch_max must not be negative",
		},
		{
			name:   "repeated embedding dimension",
			model:  ModelSpec{Name: "qwen3.7-text-embedding", Kind: "embed", Embed: EmbedParams{Dimensions: []int{1024, 1024}}},
			reason: "dimensions repeats",
		},
		{
			name:   "transcribe parameters on a generate model",
			model:  ModelSpec{Name: "qwen3.7-max", Kind: "generate", Transcribe: TranscribeParams{Context: true}},
			reason: "sets transcribe parameters on kind",
		},
		{
			name:   "negative transcription prompt limit",
			model:  ModelSpec{Name: "paraformer-v2", Kind: "transcribe", Transcribe: TranscribeParams{PromptLimit: -1}},
			reason: "must not be negative",
		},
		{
			name:   "transcription language that is not a code",
			model:  ModelSpec{Name: "paraformer-v2", Kind: "transcribe", Transcribe: TranscribeParams{Languages: []string{"zh_CN"}}},
			reason: "must be a code like zh or en",
		},
		{
			name:   "transcription languages repeat a code",
			model:  ModelSpec{Name: "paraformer-v2", Kind: "transcribe", Transcribe: TranscribeParams{Languages: []string{"zh", "zh"}}},
			reason: "repeats",
		},
		{
			name:   "media declaration with no channel",
			model:  ModelSpec{Name: "wan3.0-video", Kind: "video", Video: VideoParams{Media: &MediaParams{}}},
			reason: "media declares no channel",
		},
		{
			name: "frames and references in one declaration",
			model: ModelSpec{Name: "wan3.0-video", Kind: "video", Video: VideoParams{
				Media: &MediaParams{FirstFrame: true, ReferenceImages: 10},
			}},
			reason: "cannot be declared with reference or document channels",
		},
		{
			name: "last frame without a first",
			model: ModelSpec{Name: "wan3.0-video", Kind: "video", Video: VideoParams{
				Media: &MediaParams{LastFrame: true},
			}},
			reason: "last_frame needs first_frame",
		},
		{
			name: "media beside a flat input shape",
			model: ModelSpec{Name: "wan3.0-video", Kind: "video", Video: VideoParams{
				Input: videoInputImage, Media: &MediaParams{FirstFrame: true},
			}},
			reason: "input \"i2v\" and media cannot both be declared",
		},
		{
			name: "media beside the size parameter",
			model: ModelSpec{Name: "wan3.0-video", Kind: "video", Video: VideoParams{
				SizeParam: videoSizeConcrete, Media: &MediaParams{FirstFrame: true},
			}},
			reason: "size_param and media cannot both be declared",
		},
		{
			name: "media beside an input-audio channel",
			model: ModelSpec{Name: "wan3.0-video", Kind: "video", Video: VideoParams{
				Audio: true, Media: &MediaParams{ReferenceAudios: 5},
			}},
			reason: "audio and media cannot both be declared",
		},
		{
			name:   "smart duration without the media array",
			model:  ModelSpec{Name: "wan2.2-t2v-plus", Kind: "video", Video: VideoParams{SmartDuration: true}},
			reason: "smart_duration needs media",
		},
		{
			name: "negative reference cap",
			model: ModelSpec{Name: "wan3.0-video", Kind: "video", Video: VideoParams{
				Media: &MediaParams{ReferenceImages: -1},
			}},
			reason: "must not be negative",
		},
		{
			name: "the documented media declaration",
			model: ModelSpec{
				Name:         "wan3.0-video",
				Kind:         "video",
				Capabilities: videoCapabilities(),
				Video: VideoParams{
					Media: &MediaParams{
						ReferenceImages: 10,
						ReferenceVideos: 5,
						ReferenceAudios: 5,
						File:            true,
						Link:            true,
					},
					Resolutions:   []string{"1080P", "720P", "480P"},
					Ratios:        []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"},
					MinDuration:   2,
					MaxDuration:   30,
					AudioSwitch:   true,
					SmartDuration: true,
					PromptLimit:   20000,
				},
			},
			reason: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.model.Validate()
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() accepted the declaration")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error = %v, want substring %q", err, tc.reason)
			}
		})
	}
}

func TestValidateModelFamilyContract(t *testing.T) {
	image := ModelSpec{
		Name:         "wanx-v1",
		Kind:         "image",
		Capabilities: imageCapabilities(),
	}
	if err := validateModel(image); err != nil {
		t.Errorf("image family: %v", err)
	}
	video := ModelSpec{
		Name:         "wan2.2-t2v-plus",
		Kind:         "video",
		Capabilities: videoCapabilities(),
	}
	if err := validateModel(video); err != nil {
		t.Errorf("video family: %v", err)
	}
	// The compiler bound by kind can only produce the modality its kind
	// states, so a mismatched pair has to reject rather than compile into a
	// response the caller cannot consume.
	video.Capabilities = imageCapabilities()
	if err := validateModel(video); err == nil {
		t.Error("the video family accepted image output")
	}
	image.Capabilities = videoCapabilities()
	if err := validateModel(image); err == nil {
		t.Error("the image family accepted video output")
	}

	// The text families: text generation publishes text and reaches media
	// only through the multimodal surface; embeddings embed text.
	generate := ModelSpec{
		Name: "qwen3.7-max",
		Kind: "generate",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText},
			Outputs: []message.PartKind{message.PartText},
		},
	}
	if err := validateModel(generate); err != nil {
		t.Errorf("generate family: %v", err)
	}
	mediaOnText := generate
	mediaOnText.Capabilities.Inputs =
		[]message.PartKind{message.PartText, message.PartImage}
	if err := validateModel(mediaOnText); err == nil {
		t.Error("the text surface accepted an image input declaration")
	}
	mediaOnText.Generate.API = generateAPIMultimodal
	if err := validateModel(mediaOnText); err != nil {
		t.Errorf("multimodal surface: %v", err)
	}
	noText := generate
	noText.Capabilities.Outputs = []message.PartKind{message.PartImage}
	if err := validateModel(noText); err == nil {
		t.Error("the generate family accepted a non-text output")
	}
	embed := ModelSpec{
		Name: "qwen3.7-text-embedding",
		Kind: "embed",
		Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{message.PartText},
		},
	}
	if err := validateModel(embed); err != nil {
		t.Errorf("embed family: %v", err)
	}
	withImage := embed
	withImage.Capabilities.Inputs =
		[]message.PartKind{message.PartText, message.PartImage}
	if err := validateModel(withImage); err == nil {
		t.Error("the embed family accepted an image input declaration")
	}

	// The speech family publishes one audio stream and speaks text; its
	// declaration has to state the encoding it serves, because the canonical
	// format always carries one.
	speech := ModelSpec{
		Name: "qwen3-tts-flash",
		Kind: "tts",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText},
			Outputs: []message.PartKind{message.PartAudio},
		},
		TTS: TTSParams{
			Encoding:     media.AudioEncodingPCM16,
			SampleRateHz: 24000,
			Channels:     1,
		},
	}
	if err := validateModel(speech); err != nil {
		t.Errorf("speech family: %v", err)
	}
	noAudio := speech
	noAudio.Capabilities.Outputs = []message.PartKind{message.PartText}
	if err := validateModel(noAudio); err == nil {
		t.Error("the speech family accepted a non-audio output")
	}
	extraOutput := speech
	extraOutput.Capabilities.Outputs =
		[]message.PartKind{message.PartAudio, message.PartText}
	if err := validateModel(extraOutput); err == nil {
		t.Error("the speech family accepted a text output alongside audio")
	}
	mediaInput := speech
	mediaInput.Capabilities.Inputs =
		[]message.PartKind{message.PartText, message.PartAudio}
	if err := validateModel(mediaInput); err == nil {
		t.Error("the speech family accepted a media input declaration")
	}
	searching := speech
	searching.Capabilities.HostedWebSearch = true
	if err := validateModel(searching); err == nil {
		t.Error("the speech family accepted hosted web search")
	}
	undeclared := speech
	undeclared.TTS.Encoding = ""
	if err := validateModel(undeclared); err == nil {
		t.Error("the speech family accepted a declaration without an encoding")
	}

	// The transcription family reads one audio file and publishes text; the
	// canonical request carries one source and the result is the transcript,
	// so a declaration naming any other modality would never compile.
	transcription := ModelSpec{
		Name: "paraformer-v2",
		Kind: "transcribe",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartAudio},
			Outputs: []message.PartKind{message.PartText},
		},
		Transcribe: TranscribeParams{Context: true},
	}
	if err := validateModel(transcription); err != nil {
		t.Errorf("transcription family: %v", err)
	}
	textInput := transcription
	textInput.Capabilities.Inputs = []message.PartKind{message.PartText}
	if err := validateModel(textInput); err == nil {
		t.Error("the transcription family accepted a text input declaration")
	}
	audioOutput := transcription
	audioOutput.Capabilities.Outputs = []message.PartKind{message.PartAudio}
	if err := validateModel(audioOutput); err == nil {
		t.Error("the transcription family accepted an audio output")
	}
	thinking := transcription
	thinking.Capabilities.Reasoning =
		model.ReasoningCapability{Kind: model.ReasoningToggle}
	if err := validateModel(thinking); err == nil {
		t.Error("the transcription family accepted a reasoning channel")
	}
	embedDimensions := transcription
	embedDimensions.Capabilities.CustomEmbedDimensions = true
	if err := validateModel(embedDimensions); err == nil {
		t.Error("the transcription family accepted custom embed dimensions")
	}
	searchingTranscription := transcription
	searchingTranscription.Capabilities.HostedWebSearch = true
	if err := validateModel(searchingTranscription); err == nil {
		t.Error("the transcription family accepted hosted web search")
	}
}

func TestSpecValidate(t *testing.T) {
	if err := (Spec{Models: []ModelSpec{{
		Name:         "wanx-v1",
		Kind:         "image",
		Capabilities: imageCapabilities(),
	}}}).Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	for _, tc := range []struct {
		name   string
		spec   Spec
		reason string
	}{
		{
			name:   "base url without a scheme",
			spec:   Spec{BaseURL: "dashscope.aliyuncs.com"},
			reason: "must be an http(s) URL",
		},
		{
			name:   "duplicate model",
			spec:   Spec{Models: []ModelSpec{{Name: "wanx-v1", Kind: "image", Capabilities: imageCapabilities()}, {Name: "wanx-v1", Kind: "image", Capabilities: imageCapabilities()}}},
			reason: "duplicate model declaration",
		},
		{
			name:   "reasoning scope with surrounding whitespace",
			spec:   Spec{ReasoningScope: " dashscope/east "},
			reason: "must not have surrounding whitespace",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil {
				t.Fatal("Validate() accepted the spec")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error = %v, want substring %q", err, tc.reason)
			}
		})
	}
}

func TestProfileSpecValidate(t *testing.T) {
	if err := (ProfileSpec{
		BaseURL:     "https://dashscope-intl.aliyuncs.com",
		WorkspaceID: "ws-1",
	}).Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	if err := (ProfileSpec{BaseURL: "dashscope-intl.aliyuncs.com"}).Validate(); err == nil {
		t.Error("a scheme-less profile base url was accepted")
	}
	if err := (ProfileSpec{WorkspaceID: "ws/1"}).Validate(); err == nil {
		t.Error("a workspace id with a slash was accepted")
	}
}

func TestVideoParamsMessageHelpers(t *testing.T) {
	params := testDeclaration(t, "wan2.6-t2v").Video
	pairs := params.sizePairs()
	if !strings.Contains(pairs, "1080P 16:9 = 1920*1080") {
		t.Errorf("sizePairs() = %q", pairs)
	}
	if durations := params.durationList(); durations != "at least 2 s and at most 15 s" {
		t.Errorf("durationList() = %q", durations)
	}
	enum := testDeclaration(t, "wan2.2-t2v-plus").Video
	if got := enum.durationList(); got != "5 s" {
		t.Errorf("durationList() = %q", got)
	}
	if reason := params.durationReason("wan2.6-t2v", 1); !strings.Contains(reason, "at least 2 s and at most 15 s") {
		t.Errorf("durationReason() = %q", reason)
	}
	if reason := enum.durationReason("wan2.2-t2v-plus", 7); !strings.Contains(reason, "accepts the durations 5 s") {
		t.Errorf("durationReason() = %q", reason)
	}
	if hint := params.tierHint(); hint != "720P, 1080P" {
		t.Errorf("tierHint() = %q", hint)
	}
	if ratios := params.ratioHint(); ratios != "16:9, 9:16" {
		t.Errorf("ratioHint() = %q", ratios)
	}
	if !params.declaresRatio("16:9") || params.declaresRatio("4:3") {
		t.Error("declaresRatio did not read the size table")
	}
}

func TestCompileDropsRequestMetadata(t *testing.T) {
	// The media surfaces carry no channel for opaque caller metadata, so the
	// compile reports the field dropped rather than claiming it rode a wire
	// that has nowhere to put it.
	imageRequest := userRequest(
		[]message.Part{message.TextPart{Text: "a kite"}},
		inference.Intent{Image: &inference.ImageIntent{}},
	)
	imageRequest.RequestMetadata = map[string]string{"session_id": "s-1"}
	_, imageReport, err := compileImageWire(t, testDeclaration(t, "wanx-v1"), imageRequest)
	if err != nil {
		t.Fatalf("image compile = %v", err)
	}
	imageDecision, ok := decisionFor(imageReport, inference.FieldGenerateRequestMetadata)
	if !ok || imageDecision.Disposition != inference.Dropped {
		t.Fatalf("image metadata decision = %+v", imageDecision)
	}
	if !strings.Contains(imageDecision.Reason, "no arbitrary request metadata channel") {
		t.Errorf("image metadata reason = %q", imageDecision.Reason)
	}

	videoRequest := userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat"}},
		inference.Intent{Video: &inference.VideoIntent{}},
	)
	videoRequest.RequestMetadata = map[string]string{"session_id": "s-1"}
	_, videoReport, err := compileVideoWire(t, testDeclaration(t, "wan2.2-t2v-plus"), videoRequest)
	if err != nil {
		t.Fatalf("video compile = %v", err)
	}
	videoDecision, ok := decisionFor(videoReport, inference.FieldGenerateRequestMetadata)
	if !ok || videoDecision.Disposition != inference.Dropped {
		t.Fatalf("video metadata decision = %+v", videoDecision)
	}
	if !strings.Contains(videoDecision.Reason, "no arbitrary request metadata channel") {
		t.Errorf("video metadata reason = %q", videoDecision.Reason)
	}
}
