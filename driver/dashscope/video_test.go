package dashscope

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func TestCompileVideoText2Video(t *testing.T) {
	duration := int64(5000)
	watermark := true
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat on a river"}},
		inference.Intent{Video: &inference.VideoIntent{
			DurationMillis: &duration,
			Resolution:     "1080p",
			AspectRatio:    "16:9",
			Watermark:      &watermark,
		}},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan2.2-t2v-plus"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	body := videoRequest(wire)
	if body["model"] != "wan2.2-t2v-plus" {
		t.Errorf("model = %v", body["model"])
	}
	input := body["input"].(map[string]any)
	if input["prompt"] != "a paper boat on a river" {
		t.Errorf("input.prompt = %v", input["prompt"])
	}
	parameters := body["parameters"].(map[string]any)
	if parameters["resolution"] != "1080P" {
		t.Errorf("parameters.resolution = %v, want the uppercase tier", parameters["resolution"])
	}
	if parameters["ratio"] != "16:9" {
		t.Errorf("parameters.ratio = %v", parameters["ratio"])
	}
	if parameters["duration"] != 5 {
		t.Errorf("parameters.duration = %v, want 5", parameters["duration"])
	}
	if parameters["watermark"] != true {
		t.Errorf("parameters.watermark = %v", parameters["watermark"])
	}
	if _, ok := parameters["size"]; ok {
		t.Errorf("parameters carries size alongside the tier: %v", parameters)
	}
}

func TestCompileVideoSizeParamResolvesConcreteSize(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat"}},
		inference.Intent{Video: &inference.VideoIntent{
			Resolution:  "720p",
			AspectRatio: "9:16",
		}},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan2.6-t2v"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if !wire.sizeConcrete || wire.resolution != "720*1280" {
		t.Fatalf("wire = %#v, want the resolved concrete size", wire)
	}
	parameters := videoRequest(wire)["parameters"].(map[string]any)
	if parameters["size"] != "720*1280" {
		t.Errorf("parameters.size = %v", parameters["size"])
	}
	if _, ok := parameters["resolution"]; ok {
		t.Errorf("parameters carries resolution alongside size: %v", parameters)
	}
}

func TestCompileVideoSizeParamUndeclaredPairRejects(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat"}},
		inference.Intent{Video: &inference.VideoIntent{
			Resolution:  "1080p",
			AspectRatio: "4:3",
		}},
	)
	_, report, err := compileVideoWire(t, testDeclaration(t, "wan2.6-t2v"), request)
	if err == nil {
		t.Fatalf("compile accepted an undeclared tier/ratio pair; report = %+v", report)
	}
	reason := rejectedReason(report, inference.FieldGenerateIntentVideoAspectRatio)
	if !strings.Contains(reason, "accepts the ratios 16:9, 9:16") {
		t.Errorf("ratio rejection = %q", reason)
	}
}

func TestCompileVideoSizeParamRequiresTier(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat"}},
		inference.Intent{Video: &inference.VideoIntent{}},
	)
	_, report, err := compileVideoWire(t, testDeclaration(t, "wan2.6-t2v"), request)
	if err == nil {
		t.Fatalf("compile accepted a size-param model without a tier; report = %+v", report)
	}
	// The unrequested resolution has no active field, so the rejection lands
	// on the video intent itself.
	if reason := rejectedReason(report, inference.FieldGenerateIntentVideo); !strings.Contains(reason, "takes a concrete size") {
		t.Errorf("resolution rejection = %q", reason)
	}
}

func TestCompileVideoImageToVideo(t *testing.T) {
	duration := int64(10000)
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "she turns to the camera"},
			imagePart(t, "https://example.com/first.png"),
			audioPart(t, "https://example.com/voice.wav"),
		},
		inference.Intent{Video: &inference.VideoIntent{DurationMillis: &duration}},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan2.5-i2v-preview"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	body := videoRequest(wire)
	input := body["input"].(map[string]any)
	if input["img_url"] != "https://example.com/first.png" {
		t.Errorf("input.img_url = %v", input["img_url"])
	}
	if input["audio_url"] != "https://example.com/voice.wav" {
		t.Errorf("input.audio_url = %v", input["audio_url"])
	}
	if wire.keyframes {
		t.Error("the i2v wire selected the image2video endpoint")
	}
	parameters := body["parameters"].(map[string]any)
	if parameters["duration"] != 10 {
		t.Errorf("parameters.duration = %v", parameters["duration"])
	}
}

func TestCompileVideoImageToVideoRequiresImage(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "she turns to the camera"}},
		inference.Intent{Video: &inference.VideoIntent{}},
	)
	_, report, err := compileVideoWire(t, testDeclaration(t, "wan2.5-i2v-preview"), request)
	if err == nil {
		t.Fatalf("compile accepted an i2v model without an image; report = %+v", report)
	}
	if reason := rejectedReason(report, inference.FieldGenerateIntentVideo); !strings.Contains(reason, "requires an input image") {
		t.Errorf("input rejection = %q", reason)
	}
}

func TestCompileVideoKeyframes(t *testing.T) {
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "the canoe drifts away"},
			imagePart(t, "https://example.com/start.png"),
			imagePart(t, "https://example.com/end.png"),
		},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "720p"}},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wanx2.1-kf2v-plus"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if !wire.keyframes {
		t.Fatal("the kf2v wire did not select the image2video endpoint")
	}
	input := videoRequest(wire)["input"].(map[string]any)
	if input["first_frame_url"] != "https://example.com/start.png" {
		t.Errorf("input.first_frame_url = %v", input["first_frame_url"])
	}
	if input["last_frame_url"] != "https://example.com/end.png" {
		t.Errorf("input.last_frame_url = %v", input["last_frame_url"])
	}
	if _, ok := input["img_url"]; ok {
		t.Errorf("the kf2v payload carries img_url: %v", input)
	}
}

func TestCompileVideoT2vRejectsInputImage(t *testing.T) {
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "a paper boat"},
			imagePart(t, "https://example.com/start.png"),
		},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
	)
	_, report, err := compileVideoWire(t, testDeclaration(t, "wan2.2-t2v-plus"), request)
	if err == nil {
		t.Fatalf("compile accepted an input image on t2v; report = %+v", report)
	}
	reason := rejectedReason(report, inference.FieldGenerateInputImage)
	if !strings.Contains(reason, "takes no input image") {
		t.Errorf("image rejection = %q", reason)
	}
}

func TestCompileVideoDurationValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		millis int64
		reason string
	}{
		{name: "not whole seconds", millis: 5500, reason: "whole seconds"},
		{name: "outside the enum", millis: 7000, reason: "accepts the durations 5 s"},
		{name: "below the declared range", millis: 1000, reason: "at least 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			declaration := testDeclaration(t, "wan2.2-t2v-plus")
			if tc.name != "outside the enum" {
				declaration = testDeclaration(t, "wan2.6-t2v")
			}
			duration := tc.millis
			request := userRequest(
				[]message.Part{message.TextPart{Text: "a paper boat"}},
				inference.Intent{Video: &inference.VideoIntent{
					DurationMillis: &duration,
					Resolution:     "720p",
					AspectRatio:    "16:9",
				}},
			)
			_, report, err := compileVideoWire(t, declaration, request)
			if err == nil {
				t.Fatalf("compile accepted duration %d ms; report = %+v", tc.millis, report)
			}
			reason := rejectedReason(report, inference.FieldGenerateIntentVideoDuration)
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("duration rejection = %q, want substring %q", reason, tc.reason)
			}
		})
	}
}

func TestCompileVideoOptionGating(t *testing.T) {
	audio := true
	for _, tc := range []struct {
		name        string
		declaration string
		options     VideoOptions
		request     func() inference.GenerateRequest
		field       inference.FieldID
		reason      string
	}{
		{
			name:        "shot type on a model without the knob",
			declaration: "wan2.2-t2v-plus",
			options:     VideoOptions{Provider: providerID, ShotType: "multi"},
			request: func() inference.GenerateRequest {
				return userRequest(
					[]message.Part{message.TextPart{Text: "a paper boat"}},
					inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
				)
			},
			field:  videoOptionsField("shot_type"),
			reason: "does not declare the shot-type knob",
		},
		{
			name:        "audio switch on a model without the knob",
			declaration: "wan2.2-t2v-plus",
			options:     VideoOptions{Provider: providerID, Audio: &audio},
			request: func() inference.GenerateRequest {
				return userRequest(
					[]message.Part{message.TextPart{Text: "a paper boat"}},
					inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
				)
			},
			field:  videoOptionsField("audio"),
			reason: "does not declare an output-audio switch",
		},
		{
			name:        "template on a model without templates",
			declaration: "wan2.2-t2v-plus",
			options:     VideoOptions{Provider: providerID, Template: "flying-carpet"},
			request: func() inference.GenerateRequest {
				return userRequest(
					[]message.Part{message.TextPart{Text: "a paper boat"}},
					inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
				)
			},
			field:  videoOptionsField("template"),
			reason: "does not declare video-effect templates",
		},
		{
			name:        "audio input on a model without audio input",
			declaration: "wan2.2-t2v-plus",
			options:     VideoOptions{Provider: providerID},
			request: func() inference.GenerateRequest {
				return userRequest(
					[]message.Part{
						message.TextPart{Text: "a paper boat"},
						audioPart(t, "https://example.com/voice.wav"),
					},
					inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
				)
			},
			field:  inference.FieldGenerateInputAudio,
			reason: "does not declare audio input",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.request()
			request.Extensions = inference.Extensions{tc.options}
			_, report, err := compileVideoWire(t, testDeclaration(t, tc.declaration), request)
			if err == nil {
				t.Fatalf("compile succeeded; report = %+v", report)
			}
			reason := rejectedReason(report, tc.field)
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("rejection = %q, want substring %q", reason, tc.reason)
			}
		})
	}
}

func TestCompileVideoDeclaredKnobsLower(t *testing.T) {
	audio := true
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat"}},
		inference.Intent{Video: &inference.VideoIntent{
			Resolution:  "720p",
			AspectRatio: "16:9",
		}},
		VideoOptions{
			Provider:       providerID,
			NegativePrompt: "jitter",
			ShotType:       "multi",
			Audio:          &audio,
		},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan2.6-t2v"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	body := videoRequest(wire)
	input := body["input"].(map[string]any)
	if input["negative_prompt"] != "jitter" {
		t.Errorf("input.negative_prompt = %v", input["negative_prompt"])
	}
	parameters := body["parameters"].(map[string]any)
	if parameters["shot_type"] != "multi" {
		t.Errorf("parameters.shot_type = %v", parameters["shot_type"])
	}
	if parameters["audio"] != true {
		t.Errorf("parameters.audio = %v", parameters["audio"])
	}
}

func TestCompileVideoResolutionTierLowers(t *testing.T) {
	// A tier-param surface sends the canonical tier under its platform
	// spelling, and the decision stays native.
	declaration := testDeclaration(t, "wanx2.1-kf2v-plus")
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "the canoe drifts away"},
			imagePart(t, "https://example.com/start.png"),
		},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "720p"}},
	)
	wire, report, err := compileVideoWire(t, declaration, request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	decision, ok := decisionFor(report, inference.FieldGenerateIntentVideoResolution)
	if !ok {
		t.Fatalf("no decision for the resolution field: %+v", report.Decisions)
	}
	if decision.Disposition != inference.Native {
		t.Fatalf("resolution decision = %+v, want native", decision)
	}
	if wire.resolution != "720P" {
		t.Fatalf("wire resolution = %q, want the uppercase tier", wire.resolution)
	}
	parameters := videoRequest(wire)["parameters"].(map[string]any)
	if parameters["resolution"] != "720P" {
		t.Fatalf("parameters.resolution = %v", parameters["resolution"])
	}
}

func TestCompileVideoStreamShapeRejects(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat"}},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
	)
	compiled, err := compileVideo(testDeclarations["wan2.2-t2v-plus"], "wan2.2-t2v-plus")(
		t.Context(),
		testModelRef("wan2.2-t2v-plus"),
		request,
		inference.GenerateExecutionStream,
	)
	if err == nil {
		t.Fatal("compile accepted the stream shape")
	}
	if reason := rejectedReason(compiled.Report, inference.FieldGenerateExecutionStream); reason == "" {
		t.Errorf("stream field was not rejected; report = %+v", compiled.Report.Decisions)
	}
}

func TestConformanceGenerateCompilerVideo(t *testing.T) {
	declared := testDeclaration(t, "wan2.5-i2v-preview")
	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[videoWire]{
		Model: testModelRef("wan2.5-i2v-preview"),
		Shape: inference.GenerateExecutionUnary,
		Request: func() inference.GenerateRequest {
			return userRequest(
				[]message.Part{
					message.TextPart{Text: "she turns to the camera"},
					imagePart(t, "https://example.com/first.png"),
				},
				inference.Intent{Video: &inference.VideoIntent{Resolution: "720p"}},
			)
		},
		Snapshot: func(request inference.GenerateRequest) any { return request.Clone() },
		Compile:  compileVideo(declared, declared.Name),
		AssertWire: func(t *testing.T, wire videoWire) {
			if wire.model != "wan2.5-i2v-preview" ||
				wire.imgURL != "https://example.com/first.png" {
				t.Fatalf("wire = %#v", wire)
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
				Name: "image intent has no video surface",
				Request: func() inference.GenerateRequest {
					request := userRequest(
						[]message.Part{
							message.TextPart{Text: "she turns to the camera"},
							imagePart(t, "https://example.com/first.png"),
						},
						inference.Intent{Video: &inference.VideoIntent{Resolution: "720p"}},
					)
					request.Input.Content.Intent.Image = &inference.ImageIntent{}
					return request
				},
				Field: inference.FieldGenerateIntentImage,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "system context has no channel",
				Request: func() inference.GenerateRequest {
					request := userRequest(
						[]message.Part{
							message.TextPart{Text: "she turns to the camera"},
							imagePart(t, "https://example.com/first.png"),
						},
						inference.Intent{Video: &inference.VideoIntent{Resolution: "720p"}},
					)
					request.Context = []message.Message{{
						Role: message.RoleSystem,
						Content: message.Content{Parts: []message.Part{
							message.TextPart{Text: "be brief"},
						}},
					}}
					return request
				},
				Field: inference.FieldGenerateContextRole,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})
}

func TestVideoOptionsValidate(t *testing.T) {
	if err := (VideoOptions{Provider: providerID, ShotType: "single"}).Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
	if err := (VideoOptions{Provider: providerID, ShotType: "many"}).Validate(); err == nil {
		t.Error("unknown shot_type was accepted")
	}
	if err := (VideoOptions{Provider: providerID, NegativePrompt: strings.Repeat("x", 501)}).Validate(); err == nil {
		t.Error("over-long negative prompt was accepted")
	}
}

func TestVideoMediaType(t *testing.T) {
	if videoMediaType != "video/mp4" {
		t.Fatalf("video media type = %q", videoMediaType)
	}
	source, err := media.NewVideoURL("https://example.com/clip.mp4", videoMediaType)
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	if source.MediaType() != "video/mp4" {
		t.Errorf("media type = %q", source.MediaType())
	}
}
