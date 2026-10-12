package dashscope

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// The Wan 3.0 media surface carries the same canonical request as the flat
// 2.x surfaces and lowers it into input.media instead: typed entries in
// request order, with the declaration — not the model name — framing what a
// part may become.

func TestCompileVideoMediaFrames(t *testing.T) {
	duration := int64(10000)
	watermark := false
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "she opens the window"},
			imagePart(t, "https://example.com/first.png"),
			imagePart(t, "https://example.com/last.png"),
		},
		inference.Intent{Video: &inference.VideoIntent{
			DurationMillis: &duration,
			Resolution:     "720p",
			AspectRatio:    "16:9",
			Watermark:      &watermark,
		}},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if wire.keyframes {
		t.Error("a media declaration selected the 2.x image2video endpoint")
	}
	body := videoRequest(wire)
	assertNoFlatInput(t, body["input"].(map[string]any))
	assertMedia(t, body, []string{
		"first_frame https://example.com/first.png",
		"last_frame https://example.com/last.png",
	})
	parameters := body["parameters"].(map[string]any)
	if parameters["resolution"] != "720P" {
		t.Errorf("parameters.resolution = %v", parameters["resolution"])
	}
	if parameters["ratio"] != "16:9" {
		t.Errorf("parameters.ratio = %v", parameters["ratio"])
	}
	if parameters["duration"] != 10 {
		t.Errorf("parameters.duration = %v, want 10", parameters["duration"])
	}
	if parameters["watermark"] != false {
		t.Errorf("parameters.watermark = %v", parameters["watermark"])
	}
	if _, ok := parameters["size"]; ok {
		t.Errorf("parameters carries the 2.x size key: %v", parameters)
	}
}

func TestCompileVideoMediaFramesTakeTwoImages(t *testing.T) {
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "she opens the window"},
			imagePart(t, "https://example.com/first.png"),
			imagePart(t, "https://example.com/last.png"),
			imagePart(t, "https://example.com/extra.png"),
		},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "720p"}},
	)
	_, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video"), request)
	if err == nil {
		t.Fatalf("the frames surface accepted a third image; report = %+v", report)
	}
	reason := rejectedReason(report, inference.FieldGenerateInputImage)
	if !strings.Contains(reason, "one first frame") {
		t.Errorf("rejection = %q", reason)
	}
}

func TestCompileVideoMediaReferenceEntries(t *testing.T) {
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "video1 plays beside the still in image1"},
			imagePart(t, "https://example.com/still.png"),
			videoPart(t, "https://example.com/clip.mp4"),
			audioPart(t, "https://example.com/voice.wav"),
		},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video-reference"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if wire.model != "wan3.0-video" {
		t.Errorf("model = %q, want the wire model the alias names", wire.model)
	}
	body := videoRequest(wire)
	assertNoFlatInput(t, body["input"].(map[string]any))
	assertMedia(t, body, []string{
		"reference_image https://example.com/still.png",
		"reference_video https://example.com/clip.mp4",
		"reference_audio https://example.com/voice.wav",
	})
}

func TestCompileVideoMediaReferenceCaps(t *testing.T) {
	parts := []message.Part{message.TextPart{Text: "a crowded street"}}
	for index := 0; index < 11; index++ {
		parts = append(parts, imagePart(t, fmt.Sprintf("https://example.com/still-%d.png", index)))
	}
	request := userRequest(
		parts,
		inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
	)
	_, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video-reference"), request)
	if err == nil {
		t.Fatalf("the reference surface accepted eleven images; report = %+v", report)
	}
	reason := rejectedReason(report, inference.FieldGenerateInputImage)
	if !strings.Contains(reason, "at most 10 reference images") {
		t.Errorf("rejection = %q", reason)
	}
}

func TestCompileVideoMediaFramesRefuseOtherAssets(t *testing.T) {
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "she turns to the camera"},
			imagePart(t, "https://example.com/first.png"),
			audioPart(t, "https://example.com/voice.wav"),
		},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "720p"}},
	)
	_, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video"), request)
	if err == nil {
		t.Fatalf("the frames surface accepted an audio reference; report = %+v", report)
	}
	reason := rejectedReason(report, inference.FieldGenerateInputAudio)
	if !strings.Contains(reason, "refuses first/last frames together with other media") {
		t.Errorf("rejection = %q", reason)
	}
}

func TestCompileVideoMediaInlineImage(t *testing.T) {
	source, err := media.NewImageBytes([]byte("png-bytes"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "an apple on a table"},
			message.ImagePart{Source: source},
		},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "720p"}},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	// The media array documents image URLs and base64 data, so an inline
	// image rides its entry as a data URI.
	pairs := mediaPairs(t, videoRequest(wire))
	if len(pairs) != 1 || !strings.HasPrefix(pairs[0], "first_frame data:image/png;base64,") {
		t.Errorf("media = %v, want the inline image as a data URI", pairs)
	}
}

func TestCompileVideoMediaInlineVideoRejects(t *testing.T) {
	source, err := media.NewVideoBytes([]byte("mp4-bytes"), "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoBytes: %v", err)
	}
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "cut this clip"},
			message.VideoPart{Source: source},
		},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "720p"}},
	)
	_, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video-reference"), request)
	if err == nil {
		t.Fatalf("the media array accepted inline video bytes; report = %+v", report)
	}
	reason := rejectedReason(report, inference.FieldGenerateInputVideo)
	if !strings.Contains(reason, "have to be uploaded first") {
		t.Errorf("rejection = %q", reason)
	}
}

func TestCompileVideoMediaFileEntry(t *testing.T) {
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "turn this brief into a product film"},
			filePart(t, "https://example.com/brief.pdf"),
		},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video-reference"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	assertMedia(t, videoRequest(wire), []string{"file https://example.com/brief.pdf"})
}

func TestCompileVideoMediaFileRejections(t *testing.T) {
	for _, tc := range []struct {
		name        string
		declaration string
		parts       func(t *testing.T) []message.Part
		reason      string
	}{
		{
			name:        "a second file",
			declaration: "wan3.0-video-reference",
			parts: func(t *testing.T) []message.Part {
				return []message.Part{
					message.TextPart{Text: "a brief"},
					filePart(t, "https://example.com/one.pdf"),
					filePart(t, "https://example.com/two.pdf"),
				}
			},
			reason: "the media array takes one file",
		},
		{
			name:        "a local path",
			declaration: "wan3.0-video-reference",
			parts: func(t *testing.T) []message.Part {
				return []message.Part{
					message.TextPart{Text: "a brief"},
					filePart(t, "/tmp/brief.pdf"),
				}
			},
			reason: "has to be an http(s) or oss:// URL",
		},
		{
			name:        "a model without a file channel",
			declaration: "wan3.0-video",
			parts: func(t *testing.T) []message.Part {
				return []message.Part{
					message.TextPart{Text: "a brief"},
					filePart(t, "https://example.com/brief.pdf"),
				}
			},
			reason: "declares no file input",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := userRequest(
				tc.parts(t),
				inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
			)
			_, report, err := compileVideoWire(t, testDeclaration(t, tc.declaration), request)
			if err == nil {
				t.Fatalf("compile succeeded; report = %+v", report)
			}
			reason := rejectedReason(report, inference.FieldGenerateInputFile)
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("rejection = %q, want substring %q", reason, tc.reason)
			}
		})
	}
}

func TestCompileVideoMediaLink(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "make a film about this article"}},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
		VideoOptions{Provider: providerID, Link: "https://example.com/article"},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video-reference"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	assertMedia(t, videoRequest(wire), []string{"link https://example.com/article"})
}

func TestCompileVideoMediaLinkRejections(t *testing.T) {
	for _, tc := range []struct {
		name        string
		declaration string
		parts       func(t *testing.T) []message.Part
		reason      string
	}{
		{
			name:        "a model without web-page input",
			declaration: "wan3.0-video",
			parts: func(t *testing.T) []message.Part {
				return []message.Part{message.TextPart{Text: "a launch film"}}
			},
			reason: "declares no web-page input",
		},
		{
			name:        "a request that already carries a file",
			declaration: "wan3.0-video-reference",
			parts: func(t *testing.T) []message.Part {
				return []message.Part{
					message.TextPart{Text: "a launch film"},
					filePart(t, "https://example.com/brief.pdf"),
				}
			},
			reason: "cannot ride one request",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := userRequest(
				tc.parts(t),
				inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
				VideoOptions{Provider: providerID, Link: "https://example.com/article"},
			)
			_, report, err := compileVideoWire(t, testDeclaration(t, tc.declaration), request)
			if err == nil {
				t.Fatalf("compile succeeded; report = %+v", report)
			}
			reason := rejectedReason(report, videoOptionsField("link"))
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("rejection = %q, want substring %q", reason, tc.reason)
			}
		})
	}
}

func TestCompileVideoMediaDocumentRequiresPromptExtend(t *testing.T) {
	disabled := false
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "turn this brief into a product film"},
			filePart(t, "https://example.com/brief.pdf"),
		},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
		VideoOptions{Provider: providerID, PromptExtend: &disabled},
	)
	_, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video-reference"), request)
	if err == nil {
		t.Fatalf("the platform requires the rewrite alongside a document; report = %+v", report)
	}
	reason := rejectedReason(report, videoOptionsField("prompt_extend"))
	if !strings.Contains(reason, "requires the prompt rewrite") {
		t.Errorf("rejection = %q", reason)
	}

	enabled := true
	request.Extensions = inference.Extensions{VideoOptions{
		Provider: providerID, PromptExtend: &enabled,
	}}
	if _, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video-reference"), request); err != nil {
		t.Fatalf("the enabled rewrite was refused alongside a document: %v; report = %+v", err, report)
	}
}

func TestCompileVideoMediaSmartDuration(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat on a river"}},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "720p"}},
		VideoOptions{Provider: providerID, SmartDuration: true},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	parameters := videoRequest(wire)["parameters"].(map[string]any)
	if parameters["duration"] != -1 {
		t.Errorf("parameters.duration = %v, want the smart-duration -1", parameters["duration"])
	}

	duration := int64(5000)
	request = userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat on a river"}},
		inference.Intent{Video: &inference.VideoIntent{
			Resolution:     "720p",
			DurationMillis: &duration,
		}},
		VideoOptions{Provider: providerID, SmartDuration: true},
	)
	_, report, err = compileVideoWire(t, testDeclaration(t, "wan3.0-video"), request)
	if err == nil {
		t.Fatalf("the smart-duration option and a duration compiled together; report = %+v", report)
	}
	reason := rejectedReason(report, inference.FieldGenerateIntentVideoDuration)
	if !strings.Contains(reason, "smart-duration option decides") {
		t.Errorf("rejection = %q", reason)
	}
}

func TestCompileVideoMediaPromptOnly(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat on a river"}},
		inference.Intent{Video: &inference.VideoIntent{
			Resolution:  "480p",
			AspectRatio: "1:1",
		}},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video-reference"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	body := videoRequest(wire)
	input := body["input"].(map[string]any)
	if input["prompt"] != "a paper boat on a river" {
		t.Errorf("input.prompt = %v", input["prompt"])
	}
	// The platform takes a prompt on its own; the array is what the frames
	// and reference modes add, not a requirement of its own.
	if _, ok := input["media"]; ok {
		t.Errorf("a prompt-only request carried a media array: %v", input)
	}
	parameters := body["parameters"].(map[string]any)
	if parameters["resolution"] != "480P" || parameters["ratio"] != "1:1" {
		t.Errorf("parameters = %v", parameters)
	}
}

func TestCompileVideoMediaRequiresPromptOrMedia(t *testing.T) {
	request := userRequest(nil, inference.Intent{Video: &inference.VideoIntent{}})
	_, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video-reference"), request)
	if err == nil {
		t.Fatalf("an empty media request compiled; report = %+v", report)
	}
	reason := rejectedReason(report, inference.FieldGenerateIntentVideo)
	if !strings.Contains(reason, "requires a prompt or at least one media entry") {
		t.Errorf("rejection = %q", reason)
	}
}

func TestCompileVideoPromptLimitRejects(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: strings.Repeat("船", 20001)}},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "720p"}},
	)
	_, report, err := compileVideoWire(t, testDeclaration(t, "wan3.0-video"), request)
	if err == nil {
		t.Fatalf("the media surface accepted an over-long prompt; report = %+v", report)
	}
	reason := rejectedReason(report, inference.FieldGenerateInputText)
	if !strings.Contains(reason, "caps the prompt at 20000 characters") {
		t.Errorf("rejection = %q", reason)
	}
}

func TestCompileVideoNegativePromptGate(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat on a river"}},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "1080p"}},
		VideoOptions{Provider: providerID, NegativePrompt: "jitter"},
	)
	wire, report, err := compileVideoWire(t, testDeclaration(t, "wan2.2-t2v-plus"), request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if input := videoRequest(wire)["input"].(map[string]any); input["negative_prompt"] != "jitter" {
		t.Errorf("input.negative_prompt = %v", input["negative_prompt"])
	}

	// The media array does not document a negative prompt, so a declaration
	// that leaves the knob off has to reject it rather than send a field the
	// endpoint never named.
	_, report, err = compileVideoWire(t, testDeclaration(t, "wan3.0-video"), request)
	if err == nil {
		t.Fatalf("the media surface accepted a negative prompt; report = %+v", report)
	}
	reason := rejectedReason(report, videoOptionsField("negative_prompt"))
	if !strings.Contains(reason, "does not declare a negative prompt") {
		t.Errorf("rejection = %q", reason)
	}
}
