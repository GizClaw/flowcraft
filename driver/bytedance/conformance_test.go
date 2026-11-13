package bytedance

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
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

func TestCompileText(t *testing.T) {
	desc := testDescriptor("ark-test", "generate",
		[]message.PartKind{message.PartText}, []message.PartKind{message.PartText})
	request := testRequest("ark-test", inference.OutputText, message.NewTextContent("hello"))
	compiled, err := compileGenerate("ep-test", desc)(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.Model != "ep-test" {
		t.Fatalf("model = %q", compiled.Wire.Model)
	}
	if compiled.Wire.Instructions != nil && *compiled.Wire.Instructions != "" {
		t.Fatalf("instructions = %q, want unset without a system message", *compiled.Wire.Instructions)
	}
	if len(compiled.Fields) == 0 {
		t.Fatal("compiled fields are empty")
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
	compiled, err := compileEmbed("ep-embed", desc)(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.text == nil || compiled.Wire.text.Model != "ep-embed" {
		t.Fatalf("wire = %+v", compiled.Wire)
	}
}

func TestCompileImage(t *testing.T) {
	desc := testDescriptor("image-test", "image",
		[]message.PartKind{message.PartText}, []message.PartKind{message.PartImage})
	request := testRequest("image-test", inference.OutputImage, message.NewTextContent("a red square"))
	compiled, err := compileImage("ep-image", desc)(context.Background(), request, protocol.ShapeUnary)
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
	compiled, err := compileVideo("ep-video", desc)(context.Background(), request, protocol.ShapeUnary)
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
