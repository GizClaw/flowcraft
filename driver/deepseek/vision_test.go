package deepseek

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func visionModel() string { return "deepseek-v4-flash-vision-exp" }

func visionRequest() inference.GenerateRequest {
	source, err := media.NewImageURL(
		"https://example.com/cat.png",
		"image/png",
	)
	if err != nil {
		panic(err)
	}
	request := conformanceTextRequest()
	request.Input.Content.Parts = []message.Part{
		message.TextPart{Text: "describe this"},
		message.ImagePart{Source: source},
	}
	return request
}

func TestVisionChatAcceptsImageInput(t *testing.T) {
	compiled, err := compileChatGenerate(
		visionModel(),
		catalog[visionModel()],
	)(
		context.Background(),
		conformanceModel(visionModel()),
		visionRequest(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	last := compiled.Wire.messages[len(compiled.Wire.messages)-1]
	if len(last.content) != 2 {
		t.Fatalf("user content parts = %d, want 2", len(last.content))
	}
	if last.content[0].kind != wireContentText ||
		last.content[0].text != "describe this" {
		t.Fatalf("first content part = %+v, want text", last.content[0])
	}
	if last.content[1].kind != wireContentImage ||
		last.content[1].uri != "https://example.com/cat.png" {
		t.Fatalf("second content part = %+v, want image URL", last.content[1])
	}
}

func TestVisionResponsesAcceptsImageInput(t *testing.T) {
	compiled, err := compileResponsesGenerate(
		visionModel(),
		catalog[visionModel()],
	)(
		context.Background(),
		conformanceModel(visionModel()),
		visionRequest(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	last := compiled.Wire.items[len(compiled.Wire.items)-1]
	if last.kind != responseWireItemMessage ||
		last.role != "user" ||
		len(last.content) != 2 {
		t.Fatalf("last item = %+v, want user message with two parts", last)
	}
	if last.content[1].kind != wireContentImage ||
		last.content[1].uri != "https://example.com/cat.png" {
		t.Fatalf("image content = %+v, want image URL", last.content[1])
	}
}

func TestVisionChatInlineImageBecomesDataURL(t *testing.T) {
	source, err := media.NewImageBytes(
		[]byte{0x89, 'P', 'N', 'G'},
		"image/png",
	)
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	request := conformanceTextRequest()
	request.Input.Content.Parts = []message.Part{
		message.ImagePart{Source: source},
	}
	compiled, err := compileChatGenerate(
		visionModel(),
		catalog[visionModel()],
	)(
		context.Background(),
		conformanceModel(visionModel()),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	last := compiled.Wire.messages[len(compiled.Wire.messages)-1]
	if len(last.content) != 1 {
		t.Fatalf("user content parts = %d, want 1", len(last.content))
	}
	if !strings.HasPrefix(
		last.content[0].uri,
		"data:image/png;base64,",
	) {
		t.Fatalf("inline image uri = %q, want base64 data URL", last.content[0].uri)
	}
}

func TestVisionResponsesInlineImageBecomesDataURL(t *testing.T) {
	source, err := media.NewImageBytes(
		[]byte{0x89, 'P', 'N', 'G'},
		"image/png",
	)
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	request := conformanceTextRequest()
	request.Input.Content.Parts = []message.Part{
		message.ImagePart{Source: source},
	}
	compiled, err := compileResponsesGenerate(
		visionModel(),
		catalog[visionModel()],
	)(
		context.Background(),
		conformanceModel(visionModel()),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	last := compiled.Wire.items[len(compiled.Wire.items)-1]
	if len(last.content) != 1 {
		t.Fatalf("user content parts = %d, want 1", len(last.content))
	}
	if !strings.HasPrefix(
		last.content[0].uri,
		"data:image/png;base64,",
	) {
		t.Fatalf("inline image uri = %q, want base64 data URL", last.content[0].uri)
	}
}

func TestChatUserContentBuildsImagePart(t *testing.T) {
	parts := chatUserContent([]wireContent{
		{kind: wireContentText, text: "describe"},
		{kind: wireContentImage, uri: "https://example.com/cat.png"},
	})
	if len(parts) != 2 {
		t.Fatalf("content parts = %d, want 2", len(parts))
	}
	if parts[0].OfText == nil || parts[0].OfText.Text != "describe" {
		t.Fatalf("first content part = %+v, want text", parts[0])
	}
	if parts[1].OfImageURL == nil ||
		parts[1].OfImageURL.ImageURL.URL != "https://example.com/cat.png" {
		t.Fatalf("second content part = %+v, want image URL", parts[1])
	}
}

func TestTextModelsRejectImageInput(t *testing.T) {
	request := visionRequest()
	if _, err := compileChatGenerate(
		"deepseek-v4-flash",
		catalog["deepseek-v4-flash"],
	)(
		context.Background(),
		conformanceModel("deepseek-v4-flash"),
		request,
		inference.GenerateExecutionUnary,
	); err == nil {
		t.Fatal("chat compiler accepted an image on a text-only model")
	}
	if _, err := compileResponsesGenerate(
		"deepseek-v4-flash",
		catalog["deepseek-v4-flash"],
	)(
		context.Background(),
		conformanceModel("deepseek-v4-flash"),
		request,
		inference.GenerateExecutionUnary,
	); err == nil {
		t.Fatal("responses compiler accepted an image on a text-only model")
	}
}

// TestTextModelRejectsImageWhileDroppingEffort covers a failure report that
// carries both a rejection and a field-level drop (the medium effort maps
// to the model's "high" wire level): ValidateFailure must accept the report
// and the structured image rejection must survive.
func TestTextModelRejectsImageWhileDroppingEffort(t *testing.T) {
	request := visionRequest()
	request.Input.Content.Intent.Text = &inference.TextIntent{
		ReasoningEffort: inference.ReasoningMedium,
	}
	compiled, err := compileChatGenerate(
		"deepseek-v4-flash",
		catalog["deepseek-v4-flash"],
	)(
		context.Background(),
		conformanceModel("deepseek-v4-flash"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("chat compiler accepted an image on a text-only model")
	}
	active := request.ActiveFieldsFor(inference.GenerateExecutionUnary)
	if reportErr := compiled.Report.ValidateFailure(
		inference.OperationGenerate,
		active,
	); reportErr != nil {
		t.Fatalf("drop+reject failure report must validate: %v", reportErr)
	}
	var inferenceErr *inference.Error
	if !errors.As(err, &inferenceErr) ||
		inferenceErr.Operation != inference.OperationGenerate ||
		inferenceErr.Field != inference.FieldGenerateInputImage {
		t.Fatalf(
			"compile error = %+v, want structured rejection of %q",
			err,
			inference.FieldGenerateInputImage,
		)
	}
	if !compiled.Report.Rejects(inference.FieldGenerateInputImage) ||
		!compiled.Report.Dropped(inference.FieldGenerateIntentReasoningEffort) {
		t.Fatalf(
			"report = %+v, want image rejection with effort drop",
			compiled.Report,
		)
	}
}

func TestVisionRejectsImagesOutsideUserMessages(t *testing.T) {
	image := visionRequest().Input.Content.Parts[1]
	contextTurn := message.Message{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "ok"},
			image,
		}},
	}
	for name, compile := range map[string]inference.GenerateCompiler[chatWire]{
		"chat": compileChatGenerate(visionModel(), catalog[visionModel()]),
	} {
		request := conformanceTextRequest()
		request.Context = []message.Message{contextTurn}
		if _, err := compile(
			context.Background(),
			conformanceModel(visionModel()),
			request,
			inference.GenerateExecutionUnary,
		); err == nil {
			t.Fatalf("%s compiler accepted an image in an assistant turn", name)
		}
	}

	request := conformanceTextRequest()
	request.Context = []message.Message{contextTurn}
	if _, err := compileResponsesGenerate(
		visionModel(),
		catalog[visionModel()],
	)(
		context.Background(),
		conformanceModel(visionModel()),
		request,
		inference.GenerateExecutionUnary,
	); err == nil {
		t.Fatal("responses compiler accepted an image in an assistant turn")
	}
}

func TestVisionRejectsVideoInput(t *testing.T) {
	source, err := media.NewVideoURL(
		"https://example.com/clip.mp4",
		"video/mp4",
	)
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	request := conformanceTextRequest()
	request.Input.Content.Parts = []message.Part{
		message.VideoPart{Source: source},
	}
	for name, compile := range map[string]inference.GenerateCompiler[chatWire]{
		"chat": compileChatGenerate(visionModel(), catalog[visionModel()]),
	} {
		if _, err := compile(
			context.Background(),
			conformanceModel(visionModel()),
			request,
			inference.GenerateExecutionUnary,
		); err == nil {
			t.Fatalf("%s compiler accepted video input", name)
		}
	}
	if _, err := compileResponsesGenerate(
		visionModel(),
		catalog[visionModel()],
	)(
		context.Background(),
		conformanceModel(visionModel()),
		request,
		inference.GenerateExecutionUnary,
	); err == nil {
		t.Fatal("responses compiler accepted video input")
	}
}
