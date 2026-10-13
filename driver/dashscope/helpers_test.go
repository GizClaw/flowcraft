package dashscope

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// The declarations the compile tests build against. They mirror the shapes
// the platform documents for each surface: an async text2image model with a
// fixed size list, an editing model on the image2image surface, a sync model
// with interleaved output, and the three video input shapes.
var testDeclarations = map[string]ModelSpec{
	"wanx-v1": {
		Name: "wanx-v1",
		Kind: "image",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage},
			Outputs: []message.PartKind{message.PartImage},
		},
		Image: ImageParams{
			ReferenceImage: true,
			Style:          true,
			PromptLimit:    800,
			Sizes:          []string{"1024*1024", "720*1280", "1280*720"},
		},
	},
	"wan2.2-t2i-flash": {
		Name: "wan2.2-t2i-flash",
		Kind: "image",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText},
			Outputs: []message.PartKind{message.PartImage},
		},
		Image: ImageParams{
			PromptLimit: 500,
			MinEdge:     512,
			MaxEdge:     1440,
		},
	},
	"wanx2.1-imageedit": {
		Name: "wanx2.1-imageedit",
		Kind: "image",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage},
			Outputs: []message.PartKind{message.PartImage},
		},
		Image: ImageParams{
			Edit:        true,
			PromptLimit: 800,
			MinEdge:     512,
			MaxEdge:     4096,
		},
	},
	"wan2.6-image": {
		Name: "wan2.6-image",
		Kind: "image",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage},
			Outputs: []message.PartKind{message.PartImage, message.PartText},
		},
		Image: ImageParams{
			API:         imageAPISync,
			CountMax:    4,
			PromptLimit: 2000,
			MinPixels:   1280 * 1280,
			MaxPixels:   1440 * 1440,
		},
	},
	"wan2.2-t2v-plus": {
		Name: "wan2.2-t2v-plus",
		Kind: "video",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText},
			Outputs: []message.PartKind{message.PartVideo},
		},
		Video: VideoParams{
			Resolutions:    []string{"480P", "1080P"},
			Ratios:         []string{"16:9", "9:16"},
			Durations:      []int{5},
			NegativePrompt: true,
			PromptLimit:    800,
		},
	},
	"wan2.6-t2v": {
		Name: "wan2.6-t2v",
		Kind: "video",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText},
			Outputs: []message.PartKind{message.PartVideo},
		},
		Video: VideoParams{
			SizeParam:   videoSizeConcrete,
			Resolutions: []string{"720P", "1080P"},
			Sizes: map[string]map[string]string{
				"720P":  {"16:9": "1280*720", "9:16": "720*1280"},
				"1080P": {"16:9": "1920*1080", "9:16": "1080*1920"},
			},
			MinDuration: 2,
			MaxDuration: 15,
			AudioSwitch: true,
			ShotType:    true,
			// The 2.6 surface documents the negative prompt; 2.7 renamed
			// the knob, so the fixture keeps the older spelling.
			NegativePrompt: true,
			PromptLimit:    1500,
		},
	},
	"wan2.5-i2v-preview": {
		Name: "wan2.5-i2v-preview",
		Kind: "video",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage, message.PartAudio},
			Outputs: []message.PartKind{message.PartVideo},
		},
		Video: VideoParams{
			Input:       videoInputImage,
			Resolutions: []string{"480P", "720P", "1080P"},
			Durations:   []int{5, 10},
			Audio:       true,
			AudioSwitch: true,
			// An input soundtrack and a negative prompt ride the same
			// surface.
			NegativePrompt: true,
			PromptLimit:    1500,
		},
	},
	"wanx2.1-kf2v-plus": {
		Name: "wanx2.1-kf2v-plus",
		Kind: "video",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage},
			Outputs: []message.PartKind{message.PartVideo},
		},
		Video: VideoParams{
			Input:          videoInputKeyframes,
			SizeParam:      videoSizeResolution,
			Resolutions:    []string{"720P"},
			Template:       true,
			NegativePrompt: true,
			PromptLimit:    800,
		},
	},
	// The Wan 3.0 all-in-one model, declared twice the way a deployment
	// serves both modes: the frames surface, and the reference surface of
	// the same wire model.
	"wan3.0-video": {
		Name: "wan3.0-video",
		Kind: "video",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage},
			Outputs: []message.PartKind{message.PartVideo},
		},
		Video: VideoParams{
			Media:         &MediaParams{FirstFrame: true, LastFrame: true},
			Resolutions:   []string{"1080P", "720P", "480P"},
			Ratios:        []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"},
			MinDuration:   2,
			MaxDuration:   30,
			AudioSwitch:   true,
			SmartDuration: true,
			PromptLimit:   20000,
		},
	},
	"wan3.0-video-reference": {
		Name:      "wan3.0-video-reference",
		WireModel: "wan3.0-video",
		Kind:      "video",
		Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{
				message.PartText, message.PartImage, message.PartVideo,
				message.PartAudio, message.PartFile,
			},
			Outputs: []message.PartKind{message.PartVideo},
		},
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
	// The text-generation surfaces: qwen3.7-max rides the classic string
	// surface with the toggle thinking dial, hosted search, and the
	// max_completion_tokens lowering; qwen3.8-max rides the multimodal
	// surface with always-on thinking, media input, and reasoning replay.
	"qwen3.7-max": {
		Name: "qwen3.7-max",
		Kind: "generate",
		Capabilities: model.ModelCapabilities{
			Inputs:          []message.PartKind{message.PartText},
			Outputs:         []message.PartKind{message.PartText},
			HostedWebSearch: true,
			Reasoning:       model.ReasoningCapability{Kind: model.ReasoningToggle},
		},
		Generate: GenerateParams{MaxTokensParam: maxTokensParamCompletion},
	},
	"qwen3.8-max": {
		Name: "qwen3.8-max",
		Kind: "generate",
		Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{
				message.PartText, message.PartImage,
				message.PartVideo, message.PartAudio,
			},
			Outputs:   []message.PartKind{message.PartText},
			Reasoning: model.ReasoningCapability{Kind: model.ReasoningAlways},
		},
		Generate: GenerateParams{
			API:             generateAPIMultimodal,
			ReplayReasoning: true,
		},
	},
	"qwen3.7-text-embedding": {
		Name: "qwen3.7-text-embedding",
		Kind: "embed",
		Capabilities: model.ModelCapabilities{
			Inputs:                []message.PartKind{message.PartText},
			CustomEmbedDimensions: true,
		},
		Embed: EmbedParams{
			BatchMax:   2,
			Dimensions: []int{2560, 1024},
			Instruct:   true,
		},
	},
	// qwen3-tts-flash is the speech family: text in, one WAV file out,
	// on the multimodal-generation surface the text models use.
	"qwen3-tts-flash": {
		Name: "qwen3-tts-flash",
		Kind: "tts",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText},
			Outputs: []message.PartKind{message.PartAudio},
		},
		TTS: TTSParams{
			PromptLimit:    600,
			LanguageSelect: true,
			Encoding:       media.AudioEncodingPCM16,
			SampleRateHz:   24000,
			Channels:       1,
		},
	},
	// paraformer-v2 is the file-transcription family at its fullest: the
	// async recognition pipeline with every documented parameter declared.
	"paraformer-v2": {
		Name: "paraformer-v2",
		Kind: "transcribe",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartAudio},
			Outputs: []message.PartKind{message.PartText},
		},
		Transcribe: TranscribeParams{
			Context:           true,
			PromptLimit:       400,
			LanguageHints:     true,
			Languages:         []string{"zh", "en"},
			Timestamps:        true,
			DisfluencyRemoval: true,
			SpecialWordFilter: true,
			Vocabulary:        true,
			Channel:           true,
		},
	},
	// fun-asr is the same pipeline with a narrower parameter set: no
	// context channel and no timestamp calibration, so the canonical prompt
	// and timestamps asks have to reject against it.
	"fun-asr": {
		Name: "fun-asr",
		Kind: "transcribe",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartAudio},
			Outputs: []message.PartKind{message.PartText},
		},
		Transcribe: TranscribeParams{
			LanguageHints: true,
			Languages:     []string{"zh", "en", "ja"},
			Vocabulary:    true,
		},
	},
}

func testDeclaration(t *testing.T, name string) ModelSpec {
	t.Helper()
	declared, ok := testDeclarations[name]
	if !ok {
		t.Fatalf("no test declaration for %q", name)
	}
	return declared
}

func testModelRef(name string) model.ModelRef {
	return model.ModelRef{ID: model.ModelID{Provider: providerID, Name: name}}
}

// userRequest builds a unary generate request whose input carries the parts
// and intent under test.
func userRequest(
	parts []message.Part,
	intent inference.Intent,
	extensions ...inference.Extension,
) inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: parts},
				Intent:  intent,
			},
		},
		Extensions: append(inference.Extensions(nil), extensions...),
	}
}

func imagePart(t *testing.T, url string) message.ImagePart {
	t.Helper()
	source, err := media.NewImageURL(url, "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	return message.ImagePart{Source: source}
}

func audioPart(t *testing.T, url string) message.AudioPart {
	t.Helper()
	source, err := media.NewAudioURL(url, "audio/wav")
	if err != nil {
		t.Fatalf("NewAudioURL: %v", err)
	}
	return message.AudioPart{Source: source}
}

// audioURLSource builds a URL audio source for the transcription surface,
// which reads a source rather than a part.
func audioURLSource(t *testing.T, url string) media.AudioSource {
	t.Helper()
	source, err := media.NewAudioURL(url, "audio/wav")
	if err != nil {
		t.Fatalf("NewAudioURL: %v", err)
	}
	return source
}

// audioBytesSource builds an inline audio source: the shape the
// file-transcription surface cannot fetch and therefore rejects.
func audioBytesSource(t *testing.T, data []byte) media.AudioSource {
	t.Helper()
	source, err := media.NewAudioBytes(data, "audio/wav")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	return source
}

func videoPart(t *testing.T, url string) message.VideoPart {
	t.Helper()
	source, err := media.NewVideoURL(url, "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	return message.VideoPart{Source: source}
}

func filePart(t *testing.T, uri string) message.FilePart {
	t.Helper()
	return message.FilePart{URI: uri, MediaType: "application/pdf", Name: "brief.pdf"}
}

func compileImageWire(
	t *testing.T,
	declared ModelSpec,
	request inference.GenerateRequest,
) (imageWire, inference.CompileReport, error) {
	t.Helper()
	compiled, err := compileImage(declared, declared.Name)(
		context.Background(),
		testModelRef(declared.Name),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		return imageWire{}, compiled.Report, err
	}
	return compiled.Wire, compiled.Report, nil
}

func compileVideoWire(
	t *testing.T,
	declared ModelSpec,
	request inference.GenerateRequest,
) (videoWire, inference.CompileReport, error) {
	t.Helper()
	compiled, err := compileVideo(declared, declared.Name)(
		context.Background(),
		testModelRef(declared.Name),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		return videoWire{}, compiled.Report, err
	}
	return compiled.Wire, compiled.Report, nil
}

func compileGenerateWire(
	t *testing.T,
	declared ModelSpec,
	request inference.GenerateRequest,
	shape inference.GenerateExecutionShape,
) (generateWire, inference.CompileReport, error) {
	t.Helper()
	scope := inference.ReasoningScope("", providerID, declared.Name, "")
	compiled, err := compileGenerate(declared, scope, declared.Name)(
		context.Background(),
		testModelRef(declared.Name),
		request,
		shape,
	)
	if err != nil {
		return generateWire{}, compiled.Report, err
	}
	return compiled.Wire, compiled.Report, nil
}

func compileEmbedWire(
	t *testing.T,
	declared ModelSpec,
	request inference.EmbedRequest,
) (embedWire, inference.CompileReport, error) {
	t.Helper()
	compiled, err := compileEmbed(declared, declared.Name)(
		context.Background(),
		testModelRef(declared.Name),
		request,
	)
	if err != nil {
		return embedWire{}, compiled.Report, err
	}
	return compiled.Wire, compiled.Report, nil
}

func compileTTSWire(
	t *testing.T,
	declared ModelSpec,
	request inference.GenerateRequest,
	shape inference.GenerateExecutionShape,
) (ttsWire, inference.CompileReport, error) {
	t.Helper()
	compiled, err := compileTTS(declared, declared.Name)(
		context.Background(),
		testModelRef(declared.Name),
		request,
		shape,
	)
	if err != nil {
		return ttsWire{}, compiled.Report, err
	}
	return compiled.Wire, compiled.Report, nil
}

func compileTranscribeWire(
	t *testing.T,
	declared ModelSpec,
	request inference.TranscriptionRequest,
) (transcribeWire, inference.CompileReport, error) {
	t.Helper()
	compiled, err := compileTranscribe(declared, declared.Name)(
		context.Background(),
		testModelRef(declared.Name),
		request,
	)
	if err != nil {
		return transcribeWire{}, compiled.Report, err
	}
	return compiled.Wire, compiled.Report, nil
}

func generateOptionsField(name string) inference.FieldID {
	return inference.ExtensionField(name).Qualify(GenerateOptions{})
}

func embedOptionsField(name string) inference.FieldID {
	return inference.ExtensionField(name).Qualify(EmbedOptions{})
}

func transcribeOptionsField(name string) inference.FieldID {
	return inference.ExtensionField(name).Qualify(TranscribeOptions{})
}

// rejectedReason returns the reason recorded against a rejected field.
func rejectedReason(report inference.CompileReport, field inference.FieldID) string {
	for _, decision := range report.Decisions {
		if decision.Field == field && decision.Disposition == inference.Rejected {
			return decision.Reason
		}
	}
	return ""
}

// decisionFor returns the decision recorded against a field, if any.
func decisionFor(
	report inference.CompileReport,
	field inference.FieldID,
) (inference.Decision, bool) {
	for _, decision := range report.Decisions {
		if decision.Field == field {
			return decision, true
		}
	}
	return inference.Decision{}, false
}

func imageOptionsField(name string) inference.FieldID {
	return inference.ExtensionField(name).Qualify(ImageOptions{})
}

func videoOptionsField(name string) inference.FieldID {
	return inference.ExtensionField(name).Qualify(VideoOptions{})
}

// mediaPairs renders the input.media array of a compiled request as
// "type url" strings, in wire order.
func mediaPairs(t *testing.T, body map[string]any) []string {
	t.Helper()
	input, ok := body["input"].(map[string]any)
	if !ok {
		t.Fatalf("input = %v", body["input"])
	}
	raw, ok := input["media"].([]map[string]any)
	if !ok {
		t.Fatalf("media = %v, want the media array", input["media"])
	}
	pairs := make([]string, 0, len(raw))
	for _, entry := range raw {
		pairs = append(pairs, fmt.Sprintf("%v %v", entry["type"], entry["url"]))
	}
	return pairs
}

// flatMediaKeys are the 2.x input keys the media array replaces; a media
// request must not carry them alongside the array.
var flatMediaKeys = []string{"img_url", "first_frame_url", "last_frame_url", "audio_url"}

func assertNoFlatInput(t *testing.T, input map[string]any) {
	t.Helper()
	for _, key := range flatMediaKeys {
		if _, ok := input[key]; ok {
			t.Errorf("input carries %s alongside the media array: %v", key, input)
		}
	}
}

func assertMedia(t *testing.T, body map[string]any, want []string) {
	t.Helper()
	if got := mediaPairs(t, body); !slices.Equal(got, want) {
		t.Errorf("media = %v, want %v", got, want)
	}
}
