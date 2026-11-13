package minimax

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func speechSpec() ModelSpec {
	return testSpec("speech-2.6-hd", "tts",
		[]message.PartKind{message.PartText}, []message.PartKind{message.PartAudio})
}

func h3Spec() ModelSpec {
	return testSpec("MiniMax-H3", "video",
		[]message.PartKind{message.PartText, message.PartImage, message.PartVideo, message.PartAudio},
		[]message.PartKind{message.PartVideo})
}

func testRequest(name string, output inference.Output, content message.Content) inference.GenerateRequest {
	return inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}},
		Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: content},
		Output: output,
	}
}

func speechRequest(text string, options *TTSOptions) inference.GenerateRequest {
	request := testRequest("speech-2.6-hd", inference.OutputAudio, message.NewTextContent(text))
	if options != nil {
		request.Extensions = inference.Extensions{options}
	}
	return request
}

func compileSpeech(
	t *testing.T,
	request inference.GenerateRequest,
	shape protocol.Shape,
) (ttsWire, error) {
	t.Helper()
	compiled, err := compileTTS(speechSpec(), "speech-2.6-hd")(context.Background(), request, shape)
	return compiled.Wire, err
}

func compileH3(
	t *testing.T,
	request inference.GenerateRequest,
) (videoWire, error) {
	t.Helper()
	compiled, err := compileVideo(h3Spec(), "MiniMax-H3")(context.Background(), request, protocol.ShapeUnary)
	return compiled.Wire, err
}

func TestCompileSpeechUnary(t *testing.T) {
	wire, err := compileSpeech(t, speechRequest("hello", &TTSOptions{
		Voice: "female-shaonv", Language: "Chinese",
	}), protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.stream {
		t.Fatal("unary compile asked for a stream")
	}
	if wire.voice != "female-shaonv" || wire.language != "Chinese" {
		t.Fatalf("wire = %+v", wire)
	}
	if wire.model != "speech-2.6-hd" {
		t.Fatalf("model = %q", wire.model)
	}
	body := ttsRequest(wire)
	if body["language_boost"] != "Chinese" || body["output_format"] != "hex" {
		t.Fatalf("request = %+v", body)
	}
}

func TestCompileSpeechStream(t *testing.T) {
	wire, err := compileSpeech(t, speechRequest("hello", &TTSOptions{Voice: "v"}),
		protocol.ShapeStream)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !wire.stream {
		t.Fatal("stream compile did not ask for a stream")
	}
}

func TestCompileSpeechDefaultsToMp3(t *testing.T) {
	wire, err := compileSpeech(t, speechRequest("hello", &TTSOptions{Voice: "v"}),
		protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.format != "" {
		t.Fatalf("format = %q, want the endpoint default", wire.format)
	}
	format := ttsCanonicalFormat(wire)
	if format.Encoding != media.AudioEncodingMP3 || format.SampleRateHz != 0 {
		t.Fatalf("canonical format = %+v", format)
	}
}

func TestCompileSpeechPCMRequiresShape(t *testing.T) {
	_, err := compileSpeech(t, speechRequest("hello", &TTSOptions{
		Voice: "v", Format: media.AudioEncodingPCM16,
	}), protocol.ShapeUnary)
	if err == nil {
		t.Fatal("pcm16 without a sample rate and channels was accepted")
	}
	wire, err := compileSpeech(t, speechRequest("hello", &TTSOptions{
		Voice: "v", Format: media.AudioEncodingPCM16, SampleRateHz: 24000, Channels: 1,
	}), protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	format := ttsCanonicalFormat(wire)
	if format.Encoding != media.AudioEncodingPCM16 ||
		format.SampleRateHz != 24000 || format.Channels != 1 {
		t.Fatalf("canonical format = %+v", format)
	}
}

func TestCompileSpeechRejections(t *testing.T) {
	image, err := media.NewImageBytes([]byte("png"), "image/png")
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	cases := []struct {
		name    string
		request inference.GenerateRequest
		kind    inference.ErrorKind
	}{
		{
			name:    "missing voice",
			request: speechRequest("hello", nil),
			kind:    inference.InvalidExtension,
		},
		{
			name:    "duplicate options",
			request: withExtensions(speechRequest("hello", nil), &TTSOptions{Voice: "a"}, &TTSOptions{Voice: "b"}),
			kind:    inference.InvalidExtension,
		},
		{
			name:    "speaking an image",
			request: speechRequest("", &TTSOptions{Voice: "v"}),
			kind:    inference.UnsupportedFeature,
		},
		{
			name: "text output",
			request: testRequest("speech-2.6-hd", inference.OutputText,
				message.NewTextContent("hello")),
			kind: inference.UnsupportedFeature,
		},
	}
	// The image case needs image content, so it is built separately.
	cases[2].request.Input.Content = message.Content{Parts: []message.Part{
		message.ImagePart{Source: image},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileSpeech(t, tc.request, protocol.ShapeUnary)
			if !isErrorKind(err, tc.kind) {
				t.Fatalf("error = %v, want kind %q", err, tc.kind)
			}
		})
	}
}

func TestCompileSpeechPromptLimit(t *testing.T) {
	declared := speechSpec()
	declared.TTS = TTSParams{PromptLimit: 4}
	request := speechRequest("hello there", &TTSOptions{Voice: "v"})
	_, err := compileTTS(declared, "speech-2.6-hd")(
		context.Background(), request, protocol.ShapeUnary)
	if !isErrorKind(err, inference.InvalidRequest) {
		t.Fatalf("error = %v, want an invalid request", err)
	}
}

func TestCompileSpeechKeepsUserContextOnly(t *testing.T) {
	request := speechRequest("now", &TTSOptions{Voice: "v"})
	request.Context = []message.Message{{
		Role:    message.RoleAssistant,
		Content: message.NewTextContent("earlier"),
	}}
	_, err := compileSpeech(t, request, protocol.ShapeUnary)
	if !isErrorKind(err, inference.UnsupportedFeature) {
		t.Fatalf("error = %v, want an unsupported feature", err)
	}
}

func TestCompileVideoTextOnly(t *testing.T) {
	wire, err := compileH3(t, testRequest("MiniMax-H3", inference.OutputVideo,
		message.NewTextContent("a calm lake at dawn")))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.prompt != "a calm lake at dawn" {
		t.Fatalf("prompt = %q", wire.prompt)
	}
	// The literals are the platform's contract; comparing against the driver's own
	// constants would pass for any value they happened to hold.
	if wire.duration == nil || *wire.duration != 6 {
		t.Fatalf("duration = %v, want 6", wire.duration)
	}
	if wire.resolution != "768P" {
		t.Fatalf("resolution = %q, want 768P", wire.resolution)
	}
	if wire.ratio != "16:9" {
		t.Fatalf("ratio = %q, want the text-only default 16:9", wire.ratio)
	}
	body := videoTaskRequest(wire)
	content, ok := body["content"].([]map[string]any)
	if !ok || len(content) != 1 || content[0]["type"] != "text" {
		t.Fatalf("content = %+v", body["content"])
	}
	if body["model"] != "MiniMax-H3" {
		t.Fatalf("model = %v", body["model"])
	}
}

func TestCompileVideoOptions(t *testing.T) {
	request := testRequest("MiniMax-H3", inference.OutputVideo,
		message.NewTextContent("a calm lake"))
	request.Extensions = inference.Extensions{&VideoOptions{
		DurationMillis: int64Ptr(10000),
		Resolution:     "2K",
		AspectRatio:    "9:16",
	}}
	wire, err := compileH3(t, request)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.duration == nil || *wire.duration != 10 {
		t.Fatalf("duration = %v", wire.duration)
	}
	if wire.resolution != "2K" || wire.ratio != "9:16" {
		t.Fatalf("wire = %+v", wire)
	}
}

func TestCompileVideoRejections(t *testing.T) {
	first, err := media.NewImageURL("https://example.com/first.png", "image/png")
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	withImage := func(parts ...message.Part) inference.GenerateRequest {
		return testRequest("MiniMax-H3", inference.OutputVideo, message.Content{Parts: parts})
	}
	cases := []struct {
		name    string
		request inference.GenerateRequest
		kind    inference.ErrorKind
	}{
		{
			name:    "empty prompt",
			request: withImage(),
			kind:    inference.UnsupportedFeature,
		},
		{
			name: "duration below the envelope",
			request: withOptions(withImage(message.TextPart{Text: "a"}),
				&VideoOptions{DurationMillis: int64Ptr(3000)}),
			kind: inference.InvalidExtension,
		},
		{
			name: "duration above the envelope",
			request: withOptions(withImage(message.TextPart{Text: "a"}),
				&VideoOptions{DurationMillis: int64Ptr(16000)}),
			kind: inference.InvalidExtension,
		},
		{
			name: "partial seconds",
			request: withOptions(withImage(message.TextPart{Text: "a"}),
				&VideoOptions{DurationMillis: int64Ptr(6500)}),
			kind: inference.InvalidExtension,
		},
		{
			name: "unknown resolution",
			request: withOptions(withImage(message.TextPart{Text: "a"}),
				&VideoOptions{Resolution: "1080P"}),
			kind: inference.InvalidExtension,
		},
		{
			name: "adaptive ratio on a text-only task",
			request: withOptions(withImage(message.TextPart{Text: "a"}),
				&VideoOptions{AspectRatio: "adaptive"}),
			kind: inference.InvalidExtension,
		},
		{
			name: "first/last frame plus reference input",
			request: withImage(
				message.TextPart{Text: "a"},
				message.ImagePart{Source: first},
				message.VideoPart{Source: videoSource(t)},
			),
			kind: inference.UnsupportedFeature,
		},
		{
			name:    "audio output",
			request: testRequest("MiniMax-H3", inference.OutputAudio, message.NewTextContent("a")),
			kind:    inference.UnsupportedFeature,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileH3(t, tc.request)
			if !isErrorKind(err, tc.kind) {
				t.Fatalf("error = %v, want kind %q", err, tc.kind)
			}
		})
	}
}

func TestCompileVideoStreamRejected(t *testing.T) {
	compiled, err := compileVideo(h3Spec(), "MiniMax-H3")(context.Background(),
		testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent("a")),
		protocol.ShapeStream)
	if err == nil {
		t.Fatalf("stream compile succeeded: %+v", compiled)
	}
	if !isErrorKind(err, inference.UnsupportedFeature) {
		t.Fatalf("error = %v, want an unsupported feature", err)
	}
}

func TestCompileVideoFirstLastFrames(t *testing.T) {
	first, err := media.NewImageURL("https://example.com/first.png", "image/png")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	last, err := media.NewImageURL("https://example.com/last.png", "image/png")
	if err != nil {
		t.Fatalf("last: %v", err)
	}
	wire, err := compileH3(t, testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: []message.Part{
			message.TextPart{Text: "morph"},
			message.ImagePart{Source: first},
			message.ImagePart{Source: last},
		}}))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.firstFrame != "https://example.com/first.png" ||
		wire.lastFrame != "https://example.com/last.png" {
		t.Fatalf("wire = %+v", wire)
	}
	// An image task leaves the ratio to the server default.
	if wire.ratio != "" {
		t.Fatalf("ratio = %q, want the server default", wire.ratio)
	}
	items := videoContentItems(wire)
	if len(items) != 3 {
		t.Fatalf("content items = %d", len(items))
	}
	roles := []string{}
	for _, item := range items[1:] {
		roles = append(roles, item["role"].(string))
	}
	if strings.Join(roles, ",") != "first_frame,last_frame" {
		t.Fatalf("roles = %v", roles)
	}
}

func TestCompileVideoLastFrameOnly(t *testing.T) {
	first, err := media.NewImageURL("https://example.com/only.png", "image/png")
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	request := testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: []message.Part{
			message.TextPart{Text: "end here"},
			message.ImagePart{Source: first},
		}})
	request.Extensions = inference.Extensions{&VideoOptions{
		LastFrameOnly: boolPtr(true),
	}}
	wire, err := compileH3(t, request)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.firstFrame != "" || wire.lastFrame != "https://example.com/only.png" {
		t.Fatalf("wire = %+v", wire)
	}
}

func TestCompileVideoReferenceLimits(t *testing.T) {
	parts := []message.Part{message.TextPart{Text: "edit this"}}
	for range referenceImageMax + 1 {
		parts = append(parts, message.ImagePart{Source: imageSource(t)})
	}
	_, err := compileH3(t, testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: parts}))
	if !isErrorKind(err, inference.UnsupportedFeature) {
		t.Fatalf("error = %v, want an unsupported feature", err)
	}
}

func TestCompileVideoRejectsForeignExtension(t *testing.T) {
	request := testRequest("MiniMax-H3", inference.OutputVideo,
		message.NewTextContent("a calm lake"))
	request.Extensions = inference.Extensions{&TTSOptions{Voice: "v"}}
	_, err := compileH3(t, request)
	if !isErrorKind(err, inference.InvalidExtension) {
		t.Fatalf("error = %v, want an invalid extension", err)
	}
}

func TestTTSOptionsValidate(t *testing.T) {
	cases := []struct {
		name    string
		options TTSOptions
		ok      bool
	}{
		{name: "speed in band", options: TTSOptions{Speed: floatPtr(1.5)}, ok: true},
		{name: "speed below band", options: TTSOptions{Speed: floatPtr(0.25)}},
		{name: "speed above band", options: TTSOptions{Speed: floatPtr(2.5)}},
		{name: "unknown format", options: TTSOptions{Format: media.AudioEncodingAAC}},
		{
			name:    "sample rate outside the served set",
			options: TTSOptions{SampleRateHz: 48000},
		},
		{name: "three channels", options: TTSOptions{Channels: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.options.Validate()
			if tc.ok && err != nil {
				t.Fatalf("options rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("options accepted")
			}
		})
	}
}

func TestVideoOptionsValidate(t *testing.T) {
	cases := []struct {
		name    string
		options VideoOptions
		ok      bool
	}{
		{name: "tiers", options: VideoOptions{Resolution: "2K", AspectRatio: "16:9"}, ok: true},
		{name: "resolution", options: VideoOptions{Resolution: "1080P"}},
		{name: "ratio", options: VideoOptions{AspectRatio: "2:1"}},
		{name: "negative duration", options: VideoOptions{DurationMillis: int64Ptr(-1)}},
		{name: "callback scheme", options: VideoOptions{CallbackURL: "ftp://example.com/cb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.options.Validate()
			if tc.ok && err != nil {
				t.Fatalf("options rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("options accepted")
			}
		})
	}
}

func isErrorKind(err error, kind inference.ErrorKind) bool {
	var typed *inference.Error
	if !errors.As(err, &typed) {
		return false
	}
	return typed.Kind == kind
}

func withExtensions(request inference.GenerateRequest, extensions ...inference.Extension) inference.GenerateRequest {
	request.Extensions = inference.Extensions(extensions)
	return request
}

func withOptions(request inference.GenerateRequest, options *VideoOptions) inference.GenerateRequest {
	request.Extensions = inference.Extensions{options}
	return request
}

func imageSource(t *testing.T) media.ImageSource {
	t.Helper()
	source, err := media.NewImageURL("https://example.com/ref.png", "image/png")
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	return source
}

func videoSource(t *testing.T) media.VideoSource {
	t.Helper()
	source, err := media.NewVideoURL("https://example.com/ref.mp4", "video/mp4")
	if err != nil {
		t.Fatalf("video: %v", err)
	}
	return source
}

func audioSource(t *testing.T) media.AudioSource {
	t.Helper()
	source, err := media.NewAudioURL("https://example.com/ref.mp3", "audio/mpeg")
	if err != nil {
		t.Fatalf("audio: %v", err)
	}
	return source
}

func int64Ptr(value int64) *int64 { return &value }

func floatPtr(value float64) *float64 { return &value }

func boolPtr(value bool) *bool { return &value }

func TestWireModelAliasReachesTheWire(t *testing.T) {
	declared := speechSpec()
	declared.WireModel = "speech-2.6-hd-2026-06-01"
	request := speechRequest("hello", &TTSOptions{Voice: "v"})
	compiled, err := compileTTS(declared, "speech-2.6-hd")(
		context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.model != "speech-2.6-hd-2026-06-01" {
		t.Fatalf("wire model = %q", compiled.Wire.model)
	}
	if body := ttsRequest(compiled.Wire); body["model"] != "speech-2.6-hd-2026-06-01" {
		t.Fatalf("request model = %v", body["model"])
	}
}

func TestNewRejectsAnInconsistentLineUp(t *testing.T) {
	broken := testSpec("speech-2.6-hd", "tts", []message.PartKind{message.PartText},
		[]message.PartKind{message.PartText})
	_, err := New(Settings{BaseURL: "https://example.invalid", APIKey: "key",
		Models: []ModelSpec{broken}})
	if err == nil {
		t.Fatal("a tts model without audio output was accepted")
	}
	if !strings.Contains(err.Error(), "audio output") {
		t.Fatalf("error = %v", err)
	}
}

// TestCompileVideoRejectsUndeclaredInput pins the declaration gate: a model that
// declares text-only input must reject an image part instead of forwarding it as
// a first_frame (which would be a differently-priced operation the catalog says
// the model does not serve).
func TestCompileVideoRejectsUndeclaredInput(t *testing.T) {
	textOnly := testSpec("MiniMax-H3", "video",
		[]message.PartKind{message.PartText}, []message.PartKind{message.PartVideo})
	request := testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: []message.Part{
			message.TextPart{Text: "animate"},
			message.ImagePart{Source: imageSource(t)},
		}})
	_, err := compileVideo(textOnly, "MiniMax-H3")(
		context.Background(), request, protocol.ShapeUnary)
	if !isErrorKind(err, inference.UnsupportedFeature) {
		t.Fatalf("error = %v, want an unsupported feature", err)
	}
	if !isErrorField(err, inference.FieldContent) {
		t.Fatalf("field = %v, want content", err)
	}

	// The same request compiles once the declaration lists the image input.
	withImages := testSpec("MiniMax-H3", "video",
		[]message.PartKind{message.PartText, message.PartImage},
		[]message.PartKind{message.PartVideo})
	if _, err := compileVideo(withImages, "MiniMax-H3")(
		context.Background(), request, protocol.ShapeUnary); err != nil {
		t.Fatalf("declared image input rejected: %v", err)
	}
}

// TestCompileVideoRejectsWhitespaceOnlyPrompt covers the join hole: two empty
// text parts join to "\n", which is not an empty string but is not a prompt.
func TestCompileVideoRejectsWhitespaceOnlyPrompt(t *testing.T) {
	request := testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: []message.Part{
			message.TextPart{Text: ""},
			message.TextPart{Text: "  "},
		}})
	if _, err := compileH3(t, request); !isErrorKind(err, inference.UnsupportedFeature) {
		t.Fatalf("error = %v, want an unsupported feature", err)
	}
}

// isErrorField reports whether the error names want as its structural field.
func isErrorField(err error, want inference.Field) bool {
	var typed *inference.Error
	if !errors.As(err, &typed) {
		return false
	}
	return typed.Field == want
}

// TestCompileVideoReferenceRoles pins the multimodal reference wire shape: the
// role strings and the per-type key names are the only thing this feature does,
// and the endpoint rejects a wrong one.
func TestCompileVideoReferenceRoles(t *testing.T) {
	parts := []message.Part{message.TextPart{Text: "reference these"}}
	for range 3 {
		parts = append(parts, message.ImagePart{Source: imageSource(t)})
	}
	wire, err := compileH3(t, testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: parts}))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	items := videoContentItems(wire)
	if len(items) != 4 {
		t.Fatalf("content items = %d, want 4", len(items))
	}
	for i, item := range items[1:] {
		if item["type"] != "image_url" || item["role"] != "reference_image" {
			t.Fatalf("item %d = %+v", i, item)
		}
		if _, ok := item["image_url"].(map[string]any); !ok {
			t.Fatalf("item %d carries no image_url object: %+v", i, item)
		}
	}

	// Reference video and audio ride their own type keys.
	mixed := testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: []message.Part{
			message.TextPart{Text: "restyle this"},
			message.VideoPart{Source: videoSource(t)},
			message.AudioPart{Source: audioSource(t)},
		}})
	wire, err = compileH3(t, mixed)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	items = videoContentItems(wire)
	if len(items) != 3 {
		t.Fatalf("content items = %d, want 3", len(items))
	}
	if items[1]["type"] != "video_url" || items[1]["role"] != "reference_video" {
		t.Fatalf("video item = %+v", items[1])
	}
	if _, ok := items[1]["video_url"].(map[string]any); !ok {
		t.Fatalf("video item carries no video_url object: %+v", items[1])
	}
	if items[2]["type"] != "audio_url" || items[2]["role"] != "reference_audio" {
		t.Fatalf("audio item = %+v", items[2])
	}
	// A reference task leaves the ratio to the server.
	if wire.ratio != "" {
		t.Fatalf("ratio = %q, want the server default", wire.ratio)
	}
}

// TestCompileVideoReferenceCaps pins both media caps, not just the image one.
func TestCompileVideoReferenceCaps(t *testing.T) {
	manyVideos := []message.Part{message.TextPart{Text: "x"}}
	for range referenceVideoMax + 1 {
		manyVideos = append(manyVideos, message.VideoPart{Source: videoSource(t)})
	}
	if _, err := compileH3(t, testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: manyVideos})); !isErrorKind(err, inference.UnsupportedFeature) {
		t.Fatalf("too many videos: error = %v", err)
	}

	manyAudios := []message.Part{message.TextPart{Text: "x"}}
	for range referenceAudioMax + 1 {
		manyAudios = append(manyAudios, message.AudioPart{Source: audioSource(t)})
	}
	if _, err := compileH3(t, testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: manyAudios})); !isErrorKind(err, inference.UnsupportedFeature) {
		t.Fatalf("too many audios: error = %v", err)
	}

	// Exactly at the caps still compiles.
	atCap := []message.Part{message.TextPart{Text: "x"}}
	for range referenceVideoMax {
		atCap = append(atCap, message.VideoPart{Source: videoSource(t)})
	}
	for range referenceAudioMax {
		atCap = append(atCap, message.AudioPart{Source: audioSource(t)})
	}
	if _, err := compileH3(t, testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: atCap})); err != nil {
		t.Fatalf("at-cap request rejected: %v", err)
	}
}

// TestVideoEnforcesDeclaredInputBytes pins the byte ceiling: a declared
// max_input_bytes is a published promise, and the compiler is the only place that
// can keep it before Base64 and JSON marshal multiply the payload.
func TestVideoEnforcesDeclaredInputBytes(t *testing.T) {
	declared := h3Spec()
	declared.Limits.MaxInputBytes = int64Ptr(4)

	over := testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: []message.Part{
			message.TextPart{Text: "animate"},
			message.ImagePart{Source: mustImageBytes(t, []byte("12345678"))},
		}})
	_, err := compileVideo(declared, "MiniMax-H3")(
		context.Background(), over, protocol.ShapeUnary)
	if !isErrorKind(err, inference.InvalidRequest) {
		t.Fatalf("error = %v, want an invalid request", err)
	}
	if !isErrorField(err, inference.FieldContent) {
		t.Fatalf("field = %v, want content", err)
	}

	// Exactly at the ceiling still compiles, so the check is not off by one.
	atLimit := testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: []message.Part{
			message.TextPart{Text: "animate"},
			message.ImagePart{Source: mustImageBytes(t, []byte("1234"))},
		}})
	if _, err := compileVideo(declared, "MiniMax-H3")(
		context.Background(), atLimit, protocol.ShapeUnary); err != nil {
		t.Fatalf("at-limit input rejected: %v", err)
	}

	// A URL source carries no bytes, so the ceiling does not apply to it.
	byURL := testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: []message.Part{
			message.TextPart{Text: "animate"},
			message.ImagePart{Source: imageSource(t)},
		}})
	if _, err := compileVideo(declared, "MiniMax-H3")(
		context.Background(), byURL, protocol.ShapeUnary); err != nil {
		t.Fatalf("URL input rejected by a byte ceiling: %v", err)
	}

	// Without a declared ceiling nothing is filtered.
	undeclared := h3Spec()
	if _, err := compileVideo(undeclared, "MiniMax-H3")(
		context.Background(), over, protocol.ShapeUnary); err != nil {
		t.Fatalf("undeclared ceiling filtered a payload: %v", err)
	}
}

// TestCompileVideoUsesDeclaredEnvelope pins F15: a per-model narrowing reaches the
// compiler instead of being published and ignored.
func TestCompileVideoUsesDeclaredEnvelope(t *testing.T) {
	prompt := func(text string, options *VideoOptions) inference.GenerateRequest {
		request := testRequest("MiniMax-H3", inference.OutputVideo, message.NewTextContent(text))
		if options != nil {
			request.Extensions = inference.Extensions{options}
		}
		return request
	}

	limitedPrompt := h3Spec()
	limitedPrompt.Video = VideoParams{PromptLimit: 5}
	if _, err := compileVideo(limitedPrompt, "MiniMax-H3")(
		context.Background(), prompt("123456", nil), protocol.ShapeUnary); !isErrorKind(err, inference.InvalidRequest) {
		t.Fatalf("prompt above the declared limit: error = %v", err)
	}
	if _, err := compileVideo(limitedPrompt, "MiniMax-H3")(
		context.Background(), prompt("12345", nil), protocol.ShapeUnary); err != nil {
		t.Fatalf("prompt at the declared limit rejected: %v", err)
	}

	min := 6
	maxDuration := 8
	shortClips := h3Spec()
	shortClips.Video = VideoParams{DurationMin: &min, DurationMax: &maxDuration}
	if _, err := compileVideo(shortClips, "MiniMax-H3")(context.Background(),
		prompt("12345", &VideoOptions{DurationMillis: int64Ptr(10000)}),
		protocol.ShapeUnary); !isErrorKind(err, inference.InvalidExtension) {
		t.Fatalf("duration above the declared range: error = %v", err)
	}

	oneTier := h3Spec()
	oneTier.Video = VideoParams{Resolutions: []string{"768P"}}
	if _, err := compileVideo(oneTier, "MiniMax-H3")(context.Background(),
		prompt("12345", &VideoOptions{Resolution: "2K"}),
		protocol.ShapeUnary); !isErrorKind(err, inference.InvalidExtension) {
		t.Fatalf("undeclared tier: error = %v", err)
	}

	oneRatio := h3Spec()
	oneRatio.Video = VideoParams{Ratios: []string{"16:9"}}
	if _, err := compileVideo(oneRatio, "MiniMax-H3")(context.Background(),
		prompt("12345", &VideoOptions{AspectRatio: "9:16"}),
		protocol.ShapeUnary); !isErrorKind(err, inference.InvalidExtension) {
		t.Fatalf("undeclared ratio: error = %v", err)
	}

	oneReference := h3Spec()
	oneReference.Video = VideoParams{ReferenceImages: 2}
	parts := []message.Part{message.TextPart{Text: "12345"}}
	for range 3 {
		parts = append(parts, message.ImagePart{Source: imageSource(t)})
	}
	if _, err := compileVideo(oneReference, "MiniMax-H3")(context.Background(),
		testRequest("MiniMax-H3", inference.OutputVideo, message.Content{Parts: parts}),
		protocol.ShapeUnary); !isErrorKind(err, inference.UnsupportedFeature) {
		t.Fatalf("reference images above the declared count: error = %v", err)
	}
}

// TestCompileVideoPromptLimitBoundary pins both sides of the platform cap.
func TestCompileVideoPromptLimitBoundary(t *testing.T) {
	atLimit := strings.Repeat("a", 7000)
	if _, err := compileH3(t, testRequest("MiniMax-H3", inference.OutputVideo,
		message.NewTextContent(atLimit))); err != nil {
		t.Fatalf("7000-character prompt rejected: %v", err)
	}
	overLimit := strings.Repeat("a", 7001)
	if _, err := compileH3(t, testRequest("MiniMax-H3", inference.OutputVideo,
		message.NewTextContent(overLimit))); !isErrorKind(err, inference.InvalidRequest) {
		t.Fatalf("7001-character prompt: error = %v", err)
	}
}

// TestCompileVideoLastFrameOnlyBoundary pins both rejection conditions: the flag
// needs exactly one input image.
func TestCompileVideoLastFrameOnlyBoundary(t *testing.T) {
	only := func(request inference.GenerateRequest) inference.GenerateRequest {
		request.Extensions = inference.Extensions{&VideoOptions{LastFrameOnly: boolPtr(true)}}
		return request
	}

	// Zero images.
	none := only(testRequest("MiniMax-H3", inference.OutputVideo,
		message.NewTextContent("end here")))
	if _, err := compileH3(t, none); !isErrorKind(err, inference.InvalidExtension) {
		t.Fatalf("no image: error = %v", err)
	}

	// Two images: the flag has no single frame to mark.
	two := only(testRequest("MiniMax-H3", inference.OutputVideo,
		message.Content{Parts: []message.Part{
			message.TextPart{Text: "end here"},
			message.ImagePart{Source: imageSource(t)},
			message.ImagePart{Source: imageSource(t)},
		}}))
	if _, err := compileH3(t, two); !isErrorKind(err, inference.InvalidExtension) {
		t.Fatalf("two images: error = %v", err)
	}
	// The rejection names the qualified extension field, so a caller knows which
	// option to clear.
	if _, err := compileH3(t, two); !isErrorField(err, inference.Field("extension.minimax.video_options.last_frame_only")) {
		t.Fatalf("field = %v, want the qualified last_frame_only field", err)
	}
}

// TestProviderDispatchMatchesTheDeclaredKind pins the cross-kind rejection: a
// speech model asked for video and a video model asked for audio both fail before
// any provider I/O, naming the output.
func TestProviderDispatchMatchesTheDeclaredKind(t *testing.T) {
	provider, err := New(Settings{
		BaseURL: "https://example.invalid",
		APIKey:  "key",
		Models:  []ModelSpec{speechSpec(), h3Spec()},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		name    string
		request inference.GenerateRequest
	}{
		{
			name: "speech model asked for video",
			request: testRequest("speech-2.6-hd", inference.OutputVideo,
				message.NewTextContent("hello")),
		},
		{
			name: "video model asked for audio",
			request: testRequest("MiniMax-H3", inference.OutputAudio,
				message.NewTextContent("a calm lake")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.request.Model.ID.Name == "speech-2.6-hd" {
				tc.request.Extensions = inference.Extensions{&TTSOptions{Voice: "v"}}
			}
			_, err := provider.Generate(context.Background(), tc.request)
			if !isErrorKind(err, inference.UnsupportedFeature) {
				t.Fatalf("error = %v, want an unsupported feature", err)
			}
			if !isErrorField(err, inference.FieldOutput) {
				t.Fatalf("field = %v, want output", err)
			}
		})
	}
}
