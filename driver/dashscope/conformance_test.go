package dashscope

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func testModel(name, kind string, inputs, outputs []message.PartKind) ModelSpec {
	return ModelSpec{Name: name, Kind: kind, Capabilities: model.ModelCapabilities{Inputs: inputs, Outputs: outputs}}
}

func testGenerate(name string, output inference.Output, content message.Content) inference.GenerateRequest {
	return inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}},
		Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: content},
		Output: output,
	}
}

func TestCompileText(t *testing.T) {
	declared := testModel("qwen", "generate", []message.PartKind{message.PartText}, []message.PartKind{message.PartText})
	declared.Generate.API = generateAPIMultimodal
	request := testGenerate("qwen", inference.OutputText, message.NewTextContent("hello"))
	compiled, err := compileGenerate(declared, "qwen")(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.model != "qwen" || compiled.Wire.path != generatePathMultimodal {
		t.Fatalf("wire = %+v", compiled.Wire)
	}
	if len(compiled.Wire.messages) != 1 {
		t.Fatalf("messages = %d", len(compiled.Wire.messages))
	}
}

func TestCompileEmbed(t *testing.T) {
	declared := testModel("embed", "embed", []message.PartKind{message.PartText}, nil)
	declared.Embed = EmbedParams{BatchMax: 2}
	request := inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "embed"}},
		Input: []message.Content{message.NewTextContent("a"), message.NewTextContent("b")},
	}
	compiled, err := compileEmbed(declared, "embed")(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(compiled.Wire.batches) != 1 || len(compiled.Wire.batches[0]) != 2 {
		t.Fatalf("batches = %+v", compiled.Wire.batches)
	}
}

func TestCompileImage(t *testing.T) {
	declared := testModel("image", "image", []message.PartKind{message.PartText}, []message.PartKind{message.PartImage})
	declared.Image = ImageParams{Format: media.ImageFormatPNG}
	request := testGenerate("image", inference.OutputImage, message.NewTextContent("a red square"))
	compiled, err := compileImage(declared, "image")(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.model != "image" || compiled.Wire.prompt != "a red square" {
		t.Fatalf("wire = %+v", compiled.Wire)
	}
}

func TestCompileVideoMediaArray(t *testing.T) {
	declared := testModel("wan3", "video",
		[]message.PartKind{message.PartText, message.PartImage}, []message.PartKind{message.PartVideo})
	declared.Video = VideoParams{Media: &MediaParams{FirstFrame: true, LastFrame: true}}
	source, err := media.NewImageURL("https://example.com/first.png", "image/png")
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	request := testGenerate("wan3", inference.OutputVideo, message.Content{Parts: []message.Part{
		message.TextPart{Text: "move"}, message.ImagePart{Source: source},
	}})
	request.Extensions = inference.Extensions{&VideoOptions{Resolution: "480P", SmartDuration: true}}
	compiled, err := compileVideo(declared, "wan3")(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !compiled.Wire.mediaMode || len(compiled.Wire.media) != 1 || compiled.Wire.media[0].Type != "first_frame" {
		t.Fatalf("wire = %+v", compiled.Wire)
	}
	body := videoTaskRequest(compiled.Wire)
	input := body["input"].(map[string]any)
	if _, ok := input["img_url"]; ok {
		t.Fatal("media-array request carried img_url")
	}
	if _, ok := input["media"]; !ok {
		t.Fatal("media-array request omitted input.media")
	}
	params := body["parameters"].(map[string]any)
	if params["duration"] != -1 {
		t.Fatalf("duration = %v", params["duration"])
	}
}

func TestCompileTTS(t *testing.T) {
	declared := testModel("tts", "tts", []message.PartKind{message.PartText}, []message.PartKind{message.PartAudio})
	declared.TTS = TTSParams{Encoding: media.AudioEncodingPCM16, SampleRateHz: 24000, Channels: 1}
	request := testGenerate("tts", inference.OutputAudio, message.NewTextContent("hello"))
	request.Extensions = inference.Extensions{&TTSOptions{Voice: "Cherry", Language: "Chinese"}}
	compiled, err := compileTTS(declared, "tts")(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.voice != "Cherry" || compiled.Wire.language != "Chinese" {
		t.Fatalf("wire = %+v", compiled.Wire)
	}
}

func TestCompileTranscribe(t *testing.T) {
	declared := testModel("asr", "transcribe", []message.PartKind{message.PartAudio}, []message.PartKind{message.PartText})
	source, err := media.NewAudioURL("https://example.com/a.wav", "audio/wav")
	if err != nil {
		t.Fatalf("audio: %v", err)
	}
	request := testGenerate("asr", inference.OutputText, message.Content{Parts: []message.Part{
		message.AudioPart{Source: source},
	}})
	compiled, err := compileTranscribe(declared, "asr")(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.model != "asr" || compiled.Wire.fileURL != "https://example.com/a.wav" {
		t.Fatalf("wire = %+v", compiled.Wire)
	}
}
