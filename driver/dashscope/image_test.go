package dashscope

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func TestCompileImageText2Image(t *testing.T) {
	size := media.ImageSize{Width: 1280, Height: 720}
	count := 2
	seed := int64(42)
	watermark := false
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a lantern festival"}},
		inference.Intent{Image: &inference.ImageIntent{
			Size:     &size,
			Count:    &count,
			Seed:     &seed,
			Delivery: media.SourceURL,
		}},
		ImageOptions{Provider: providerID, NegativePrompt: "blurry", Watermark: &watermark},
	)

	wire, report, err := compileImageWire(t, testDeclaration(t, "wanx-v1"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if wire.model != "wanx-v1" || wire.edit || wire.sync {
		t.Fatalf("wire = %#v, want the async text2image surface", wire)
	}
	body := imageRequest(wire)
	if body["model"] != "wanx-v1" {
		t.Errorf("model = %v", body["model"])
	}
	input, ok := body["input"].(map[string]any)
	if !ok {
		t.Fatalf("input = %#v", body["input"])
	}
	if input["prompt"] != "a lantern festival" {
		t.Errorf("input.prompt = %v", input["prompt"])
	}
	if input["negative_prompt"] != "blurry" {
		t.Errorf("input.negative_prompt = %v", input["negative_prompt"])
	}
	parameters, ok := body["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters = %#v", body["parameters"])
	}
	if parameters["size"] != "1280*720" {
		t.Errorf("parameters.size = %v, want 1280*720", parameters["size"])
	}
	if parameters["n"] != 2 {
		t.Errorf("parameters.n = %v, want 2", parameters["n"])
	}
	if parameters["seed"] != int64(42) {
		t.Errorf("parameters.seed = %v, want 42", parameters["seed"])
	}
	if parameters["watermark"] != false {
		t.Errorf("parameters.watermark = %v, want false", parameters["watermark"])
	}
	if wire.format != media.ImageFormatPNG {
		t.Errorf("format = %q, want the family default png", wire.format)
	}
}

func TestCompileImageDefaultsCountToOne(t *testing.T) {
	// The platform default is four images, which bills four; the driver sends
	// one unless the intent asks for more.
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a paper crane"}},
		inference.Intent{Image: &inference.ImageIntent{}},
	)
	wire, report, err := compileImageWire(t, testDeclaration(t, "wanx-v1"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	parameters := imageRequest(wire)["parameters"].(map[string]any)
	if parameters["n"] != 1 {
		t.Errorf("parameters.n = %v, want the default 1", parameters["n"])
	}
}

func TestCompileImageReferenceMode(t *testing.T) {
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "stained glass"},
			imagePart(t, "https://example.com/reference.png"),
		},
		inference.Intent{Image: &inference.ImageIntent{}},
		ImageOptions{Provider: providerID, Style: "<photography>", RefMode: "refonly"},
	)
	wire, report, err := compileImageWire(t, testDeclaration(t, "wanx-v1"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	input := imageRequest(wire)["input"].(map[string]any)
	if input["ref_img"] != "https://example.com/reference.png" {
		t.Errorf("input.ref_img = %v", input["ref_img"])
	}
	parameters := imageRequest(wire)["parameters"].(map[string]any)
	if parameters["style"] != "<photography>" {
		t.Errorf("parameters.style = %v", parameters["style"])
	}
	if wire.refMode != "refonly" {
		t.Errorf("refMode = %q", wire.refMode)
	}
}

func TestCompileImageEditSurface(t *testing.T) {
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "make it snow"},
			imagePart(t, "https://example.com/base.png"),
			imagePart(t, "https://example.com/mask.png"),
		},
		inference.Intent{Image: &inference.ImageIntent{}},
		ImageOptions{Provider: providerID, Function: "description_edit_with_mask"},
	)
	wire, report, err := compileImageWire(t, testDeclaration(t, "wanx2.1-imageedit"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if !wire.edit {
		t.Fatal("wire did not select the editing surface")
	}
	input := imageRequest(wire)["input"].(map[string]any)
	if input["function"] != "description_edit_with_mask" {
		t.Errorf("input.function = %v", input["function"])
	}
	if input["base_image_url"] != "https://example.com/base.png" {
		t.Errorf("input.base_image_url = %v", input["base_image_url"])
	}
	if input["mask_image_url"] != "https://example.com/mask.png" {
		t.Errorf("input.mask_image_url = %v", input["mask_image_url"])
	}
}

func TestCompileImageEditRequiresFunction(t *testing.T) {
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "expand"},
			imagePart(t, "https://example.com/base.png"),
		},
		inference.Intent{Image: &inference.ImageIntent{}},
	)
	_, report, err := compileImageWire(t, testDeclaration(t, "wanx2.1-imageedit"), request)
	if err == nil {
		t.Fatalf("compile succeeded without the edit function; report = %+v", report)
	}
	// The function the caller left unset has no active field, so the
	// rejection lands on the image the operation would run on.
	if reason := rejectedReason(report, inference.FieldGenerateInputImage); !strings.Contains(reason, "requires the edit function") {
		t.Errorf("function rejection = %q", reason)
	}
}

func TestCompileImageSyncInterleave(t *testing.T) {
	enable := true
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "a travel poster"},
			imagePart(t, "https://example.com/style.png"),
		},
		inference.Intent{
			Text:  &inference.TextIntent{},
			Image: &inference.ImageIntent{},
		},
		ImageOptions{Provider: providerID, EnableInterleave: &enable},
	)
	wire, report, err := compileImageWire(t, testDeclaration(t, "wan2.6-image"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if !wire.sync || !wire.enableInterleave {
		t.Fatalf("wire = %#v, want the sync interleaved surface", wire)
	}
	input := imageRequest(wire)["input"].(map[string]any)
	messages, ok := input["messages"].([]map[string]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("input.messages = %#v", input["messages"])
	}
	content, ok := messages[0]["content"].([]map[string]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content = %#v", messages[0]["content"])
	}
	if content[0]["text"] != "a travel poster" {
		t.Errorf("content[0] = %#v", content[0])
	}
	if content[1]["image"] != "https://example.com/style.png" {
		t.Errorf("content[1] = %#v", content[1])
	}
	parameters := imageRequest(wire)["parameters"].(map[string]any)
	if parameters["enable_interleave"] != true {
		t.Errorf("parameters.enable_interleave = %v", parameters["enable_interleave"])
	}
	if parameters["stream"] != true {
		t.Errorf("parameters.stream = %v, want the flag paired with the sse header",
			parameters["stream"])
	}
}

func TestCompileImageRejections(t *testing.T) {
	inline := media.SourceInline
	format := media.ImageFormatJPEG
	for _, tc := range []struct {
		name        string
		declaration string
		request     func(t *testing.T) inference.GenerateRequest
		field       inference.FieldID
		reason      string
	}{
		{
			name:        "size outside the declared list",
			declaration: "wanx-v1",
			request: func(*testing.T) inference.GenerateRequest {
				size := media.ImageSize{Width: 1000, Height: 1000}
				return userRequest(
					[]message.Part{message.TextPart{Text: "a kite"}},
					inference.Intent{Image: &inference.ImageIntent{Size: &size}},
				)
			},
			field:  inference.FieldGenerateIntentImageSize,
			reason: "accepted sizes are 1024*1024",
		},
		{
			name:        "count above the declared cap",
			declaration: "wan2.6-image",
			request: func(*testing.T) inference.GenerateRequest {
				count := 5
				return userRequest(
					[]message.Part{message.TextPart{Text: "a kite"}},
					inference.Intent{Image: &inference.ImageIntent{Count: &count}},
				)
			},
			field:  inference.FieldGenerateIntentImageCount,
			reason: "at most 4 images",
		},
		{
			name:        "format the model does not deliver",
			declaration: "wanx-v1",
			request: func(*testing.T) inference.GenerateRequest {
				return userRequest(
					[]message.Part{message.TextPart{Text: "a kite"}},
					inference.Intent{Image: &inference.ImageIntent{OutputFormat: format}},
				)
			},
			field:  inference.FieldGenerateIntentImageOutputFormat,
			reason: "delivers png",
		},
		{
			name:        "inline delivery",
			declaration: "wanx-v1",
			request: func(*testing.T) inference.GenerateRequest {
				return userRequest(
					[]message.Part{message.TextPart{Text: "a kite"}},
					inference.Intent{Image: &inference.ImageIntent{Delivery: inline}},
				)
			},
			field:  inference.FieldGenerateIntentImageDelivery,
			reason: "deliver provider URLs",
		},
		{
			name:        "input image on a model without an image channel",
			declaration: "wan2.2-t2i-flash",
			request: func(t *testing.T) inference.GenerateRequest {
				return userRequest(
					[]message.Part{
						message.TextPart{Text: "a kite"},
						imagePart(t, "https://example.com/a.png"),
					},
					inference.Intent{Image: &inference.ImageIntent{}},
				)
			},
			field:  inference.FieldGenerateInputImage,
			reason: "no input-image channel",
		},
		{
			name:        "audio part on an image model",
			declaration: "wanx-v1",
			request: func(t *testing.T) inference.GenerateRequest {
				return userRequest(
					[]message.Part{
						message.TextPart{Text: "a kite"},
						audioPart(t, "https://example.com/a.wav"),
					},
					inference.Intent{Image: &inference.ImageIntent{}},
				)
			},
			field:  inference.FieldGenerateInputAudio,
			reason: "accepts text and image parts",
		},
		{
			name:        "text intent without interleave",
			declaration: "wanx-v1",
			request: func(*testing.T) inference.GenerateRequest {
				return userRequest(
					[]message.Part{message.TextPart{Text: "a kite"}},
					inference.Intent{
						Text:  &inference.TextIntent{},
						Image: &inference.ImageIntent{},
					},
				)
			},
			field:  inference.FieldGenerateIntentText,
			reason: "return images",
		},
		{
			name:        "missing prompt",
			declaration: "wanx-v1",
			request: func(*testing.T) inference.GenerateRequest {
				return userRequest(
					nil,
					inference.Intent{Image: &inference.ImageIntent{}},
				)
			},
			field:  inference.FieldGenerateIntentImage,
			reason: "requires a prompt",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, report, err := compileImageWire(
				t, testDeclaration(t, tc.declaration), tc.request(t))
			if err == nil {
				t.Fatalf("compile succeeded; report = %+v", report)
			}
			reason := rejectedReason(report, tc.field)
			if reason == "" {
				t.Fatalf("field %q was not rejected; report = %+v", tc.field, report.Decisions)
			}
			if tc.reason != "" && !strings.Contains(reason, tc.reason) {
				t.Errorf("rejection reason = %q, want substring %q", reason, tc.reason)
			}
		})
	}
}

func TestCompileImageOptionGating(t *testing.T) {
	strength := 0.5
	interleave := true
	for _, tc := range []struct {
		name        string
		declaration string
		options     ImageOptions
		field       inference.FieldID
		reason      string
	}{
		{
			name:        "style on a model without the knob",
			declaration: "wan2.2-t2i-flash",
			options:     ImageOptions{Provider: providerID, Style: "<photography>"},
			field:       imageOptionsField("style"),
			reason:      "does not declare the wanx-v1 style knob",
		},
		{
			name:        "edit function on a model that cannot edit",
			declaration: "wanx-v1",
			options:     ImageOptions{Provider: providerID, Function: "expand"},
			field:       imageOptionsField("function"),
			reason:      "belongs to the editing surface",
		},
		{
			name:        "reference strength without the reference channel",
			declaration: "wan2.2-t2i-flash",
			options:     ImageOptions{Provider: providerID, RefStrength: &strength},
			field:       imageOptionsField("ref_strength"),
			reason:      "has no reference-image channel",
		},
		{
			name:        "reference mode without the reference channel",
			declaration: "wan2.2-t2i-flash",
			options:     ImageOptions{Provider: providerID, RefMode: "refonly"},
			field:       imageOptionsField("ref_mode"),
			reason:      "has no reference-image channel",
		},
		{
			name:        "interleave on the async task surface",
			declaration: "wanx-v1",
			options:     ImageOptions{Provider: providerID, EnableInterleave: &interleave},
			field:       imageOptionsField("enable_interleave"),
			reason:      "belongs to the sync surface",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := userRequest(
				[]message.Part{message.TextPart{Text: "a kite"}},
				inference.Intent{Image: &inference.ImageIntent{}},
			)
			request.Extensions = inference.Extensions{tc.options}
			_, report, err := compileImageWire(t, testDeclaration(t, tc.declaration), request)
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

func TestCompileRejectsOtherOperationExtension(t *testing.T) {
	for _, tc := range []struct {
		name      string
		extension inference.Extension
		field     inference.FieldID
		compile   func(t *testing.T, request inference.GenerateRequest) (inference.CompileReport, error)
	}{
		{
			name: "image options on a video request",
			extension: ImageOptions{
				Provider:       providerID,
				NegativePrompt: "blurry",
			},
			field: inference.ExtensionField("negative_prompt").
				Qualify(ImageOptions{Provider: providerID}),
			compile: func(t *testing.T, request inference.GenerateRequest) (inference.CompileReport, error) {
				_, report, err := compileVideoWire(
					t, testDeclaration(t, "wan2.2-t2v-plus"), request)
				return report, err
			},
		},
		{
			name: "video options on an image request",
			extension: VideoOptions{
				Provider:       providerID,
				NegativePrompt: "blurry",
			},
			field: inference.ExtensionField("negative_prompt").
				Qualify(VideoOptions{Provider: providerID}),
			compile: func(t *testing.T, request inference.GenerateRequest) (inference.CompileReport, error) {
				_, report, err := compileImageWire(
					t, testDeclaration(t, "wanx-v1"), request)
				return report, err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := userRequest(
				[]message.Part{message.TextPart{Text: "a kite"}},
				inference.Intent{Image: &inference.ImageIntent{}},
			)
			request.Extensions = inference.Extensions{tc.extension}
			report, err := tc.compile(t, request)
			if err == nil {
				t.Fatalf("compile succeeded; report = %+v", report)
			}
			reason := rejectedReason(report, tc.field)
			if !strings.Contains(reason, "does not apply to") {
				t.Errorf("rejection = %q", reason)
			}
		})
	}
}

func TestCompileImageQualityDrops(t *testing.T) {
	for _, quality := range []media.ImageQuality{
		media.ImageQualityAuto,
		media.ImageQualityHigh,
		media.ImageQualityMax,
	} {
		request := userRequest(
			[]message.Part{message.TextPart{Text: "a kite"}},
			inference.Intent{Image: &inference.ImageIntent{Quality: quality}},
		)
		_, report, err := compileImageWire(t, testDeclaration(t, "wanx-v1"), request)
		if err != nil {
			t.Fatalf("quality %q: compile = %v", quality, err)
		}
		decision, ok := decisionFor(report, inference.FieldGenerateIntentImageQuality)
		if !ok || decision.Disposition != inference.Dropped {
			t.Fatalf("quality %q: decision = %+v", quality, decision)
		}
		if !strings.Contains(decision.Reason, "no quality parameter") {
			t.Errorf("quality %q: reason = %q", quality, decision.Reason)
		}
	}
}

func TestCompileImagePromptLimit(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: strings.Repeat("长", 801)}},
		inference.Intent{Image: &inference.ImageIntent{}},
	)
	_, report, err := compileImageWire(t, testDeclaration(t, "wanx-v1"), request)
	if err == nil {
		t.Fatalf("compile accepted an over-long prompt; report = %+v", report)
	}
	if reason := rejectedReason(report, inference.FieldGenerateInputText); !strings.Contains(reason, "800 characters") {
		t.Errorf("prompt rejection = %q", reason)
	}
}

func TestCompileImageInclinedSyncSizeEnvelope(t *testing.T) {
	// The sync surface declares a pixel envelope rather than a size list, so
	// a size inside it lowers verbatim and one below it rejects.
	inside := media.ImageSize{Width: 1328, Height: 1328}
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a poster"}},
		inference.Intent{Image: &inference.ImageIntent{Size: &inside}},
	)
	wire, report, err := compileImageWire(t, testDeclaration(t, "wan2.6-image"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if wire.size != "1328*1328" {
		t.Errorf("size = %q", wire.size)
	}

	below := media.ImageSize{Width: 1024, Height: 1024}
	request = userRequest(
		[]message.Part{message.TextPart{Text: "a poster"}},
		inference.Intent{Image: &inference.ImageIntent{Size: &below}},
	)
	_, report, err = compileImageWire(t, testDeclaration(t, "wan2.6-image"), request)
	if err == nil {
		t.Fatalf("compile accepted a size below the envelope; report = %+v", report)
	}
	reason := rejectedReason(report, inference.FieldGenerateIntentImageSize)
	if !strings.Contains(reason, "at least 1638400 pixels") {
		t.Errorf("size rejection = %q", reason)
	}
}

func TestCompileImageInlineBytesBecomeDataURI(t *testing.T) {
	source, err := media.NewImageBytes([]byte("fake-png"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "restore this"},
			message.ImagePart{Source: source},
		},
		inference.Intent{Image: &inference.ImageIntent{}},
		ImageOptions{Provider: providerID, Function: "super_resolution"},
	)
	wire, report, err := compileImageWire(t, testDeclaration(t, "wanx2.1-imageedit"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	want := "data:image/png;base64," + "ZmFrZS1wbmc="
	if wire.images[0] != want {
		t.Errorf("image = %q, want %q", wire.images[0], want)
	}
}

func TestCompileImageStreamShapeRejects(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a kite"}},
		inference.Intent{Image: &inference.ImageIntent{}},
	)
	compiled, err := compileImage(testDeclarations["wanx-v1"], "wanx-v1")(
		t.Context(),
		testModelRef("wanx-v1"),
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

func TestConformanceGenerateCompilerImage(t *testing.T) {
	declared := testDeclaration(t, "wanx-v1")
	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[imageWire]{
		Model: testModelRef("wanx-v1"),
		Shape: inference.GenerateExecutionUnary,
		Request: func() inference.GenerateRequest {
			size := media.ImageSize{Width: 1024, Height: 1024}
			return userRequest(
				[]message.Part{message.TextPart{Text: "a kite"}},
				inference.Intent{Image: &inference.ImageIntent{Size: &size}},
			)
		},
		Snapshot: func(request inference.GenerateRequest) any { return request.Clone() },
		Compile:  compileImage(declared, declared.Name),
		AssertWire: func(t *testing.T, wire imageWire) {
			if wire.model != "wanx-v1" || wire.prompt != "a kite" {
				t.Fatalf("wire = %#v", wire)
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
				Name: "video intent has no image surface",
				Request: func() inference.GenerateRequest {
					request := userRequest(
						[]message.Part{message.TextPart{Text: "a kite"}},
						inference.Intent{Image: &inference.ImageIntent{}},
					)
					request.Input.Content.Intent.Video = &inference.VideoIntent{}
					return request
				},
				Field: inference.FieldGenerateIntentVideo,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "assistant context has no channel",
				Request: func() inference.GenerateRequest {
					request := userRequest(
						[]message.Part{message.TextPart{Text: "a kite"}},
						inference.Intent{Image: &inference.ImageIntent{}},
					)
					request.Context = []message.Message{{
						Role: message.RoleAssistant,
						Content: message.Content{Parts: []message.Part{
							message.TextPart{Text: "earlier"},
						}},
					}}
					return request
				},
				Field: inference.FieldGenerateContextRole,
				Kind:  inference.UnsupportedFeature,
			},
		},
		Drops: []inferencetest.CompilerDrop[inference.GenerateRequest]{
			{
				Name: "quality",
				Request: func() inference.GenerateRequest {
					request := userRequest(
						[]message.Part{message.TextPart{Text: "a kite"}},
						inference.Intent{Image: &inference.ImageIntent{
							Quality: media.ImageQualityHigh,
						}},
					)
					return request
				},
				Field: inference.FieldGenerateIntentImageQuality,
			},
		},
	})
}

func TestImageOptionsValidate(t *testing.T) {
	if err := (ImageOptions{Provider: providerID, RefMode: "repaint"}).Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
	if err := (ImageOptions{Provider: providerID, RefMode: "stencil"}).Validate(); err == nil {
		t.Error("unknown ref_mode was accepted")
	}
	if err := (ImageOptions{Provider: providerID, RefStrength: ptrTo(1.5)}).Validate(); err == nil {
		t.Error("out-of-range ref_strength was accepted")
	}
	if err := (ImageOptions{Provider: providerID, NegativePrompt: strings.Repeat("x", 501)}).Validate(); err == nil {
		t.Error("over-long negative prompt was accepted")
	}
	if got := (ImageOptions{}).ProviderID(); got != providerID {
		t.Errorf("ProviderID() = %q", got)
	}
	if got := (ImageOptions{Provider: "custom"}).ProviderID(); got != "custom" {
		t.Errorf("ProviderID() = %q", got)
	}
}

func ptrTo[T any](value T) *T { return &value }
