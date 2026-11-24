package bytedance

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/inference/pipeline"
)

func testDescriptor(name, kind string, inputs, outputs []message.PartKind) model.ModelDescriptor {
	return descriptor(ModelSpec{
		Name: name, Kind: kind,
		Capabilities: model.ModelCapabilities{Inputs: inputs, Outputs: outputs},
	})
}

func testRequest(name string, output inference.Output, content message.Content) inference.GenerateRequest {
	return inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}},
		Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: content},
		Output: output,
	}
}

func TestCompileEmbed(t *testing.T) {
	desc := testDescriptor("embed-test", "embed",
		[]message.PartKind{message.PartText}, nil)
	desc.Capabilities.EmbeddingDimensions = 1024
	request := inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "embed-test"}},
		Input: []message.Content{message.NewTextContent("hello")},
	}
	compiled, err := compileEmbed("ep-embed", desc)(context.Background(), request, pipeline.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.text == nil || compiled.Wire.text.Model != "ep-embed" {
		t.Fatalf("wire = %+v", compiled.Wire)
	}
}

// TestCompileEmbedRejectsWhatTheSurfaceCannotCarry pins the two guards on the
// text surface: it carries one text per item, and it does not carry media at
// all. A vision model is declared with image input, which is what routes the
// driver to the fused endpoint instead.
func TestCompileEmbedRejectsWhatTheSurfaceCannotCarry(t *testing.T) {
	desc := testDescriptor("embed-test", "embed", []message.PartKind{message.PartText}, nil)
	desc.Capabilities.EmbeddingDimensions = 1024
	compile := compileEmbed("ep-embed", desc)
	twoParts := inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "embed-test"}},
		Input: []message.Content{{Parts: []message.Part{
			message.TextPart{Text: "one"},
			message.TextPart{Text: "two"},
		}}},
	}
	if _, err := compile(context.Background(), twoParts, pipeline.ShapeUnary); err == nil {
		t.Fatal("two text parts in one item were accepted")
	}
	withImage := inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "embed-test"}},
		Input: []message.Content{{
			Parts: []message.Part{
				message.TextPart{Text: "one"},
				embedTestImagePart(t),
			},
		}},
	}
	if _, err := compile(context.Background(), withImage, pipeline.ShapeUnary); err == nil {
		t.Fatal("an image was accepted by a text-only model")
	}
}

// TestCompileEmbedFusesTextAndImage is the multimodal surface: one declared as
// accepting images puts text and media into the same item.
func TestCompileEmbedFusesTextAndImage(t *testing.T) {
	desc := testDescriptor("embed-vision", "embed",
		[]message.PartKind{message.PartText, message.PartImage}, nil)
	desc.Capabilities.EmbeddingDimensions = 2048
	request := inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "embed-vision"}},
		Input: []message.Content{{
			Parts: []message.Part{
				message.TextPart{Text: "a cat"},
				embedTestImagePart(t),
			},
		}},
	}
	compiled, err := compileEmbed("ep-embed", desc)(context.Background(), request, pipeline.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.text != nil {
		t.Fatal("a fused request went to the text surface")
	}
	if len(compiled.Wire.multimodal) != 1 || len(compiled.Wire.multimodal[0].Input) != 2 {
		t.Fatalf("fused = %+v", compiled.Wire.multimodal)
	}
	if compiled.Wire.multimodal[0].Input[1].ImageURL == nil {
		t.Fatalf("second item = %+v, want an image", compiled.Wire.multimodal[0].Input[1])
	}
}

// embedTestImagePart is one inline image for the embed tests.
func embedTestImagePart(tb testing.TB) message.Part {
	tb.Helper()
	source, err := media.NewImageBytes([]byte("png"), "image/png")
	if err != nil {
		tb.Fatalf("NewImageBytes: %v", err)
	}
	return message.ImagePart{Source: source}
}

func TestCompileImage(t *testing.T) {
	desc := testDescriptor("image-test", "image",
		[]message.PartKind{message.PartText}, []message.PartKind{message.PartImage})
	request := testRequest("image-test", inference.OutputImage, message.NewTextContent("a red square"))
	compiled, err := compileImage("ep-image", desc)(context.Background(), request, pipeline.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.ark.Model != "ep-image" || compiled.Wire.ark.Prompt != "a red square" {
		t.Fatalf("wire = %+v", compiled.Wire.ark)
	}
}

func TestCompileVideo(t *testing.T) {
	desc := ModelSpec{
		Name: "video-test", Kind: "video",
		Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{message.PartText}, Outputs: []message.PartKind{message.PartVideo},
		},
	}
	request := testRequest("video-test", inference.OutputVideo, message.NewTextContent("a calm lake"))
	compiled, err := compileVideo("ep-video", desc)(context.Background(), request, pipeline.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.Model != "ep-video" {
		t.Fatalf("model = %q", compiled.Wire.Model)
	}
	if len(compiled.Wire.Content) == 0 || compiled.Wire.Content[0].Text == nil {
		t.Fatalf("content = %+v", compiled.Wire.Content)
	}
}
