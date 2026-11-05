package openai

import (
	"context"
	"encoding/base64"
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// mediaRequest builds a one-turn request whose input carries extra content parts.
func mediaRequest(t *testing.T, parts ...message.Part) inference.GenerateRequest {
	t.Helper()
	request := inference.GenerateRequest{
		Model: model.ModelRef{
			ID:      model.ModelID{Provider: ProviderID, Name: "gpt-5-mini"},
			Profile: "default",
		},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.Content{Parts: parts},
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

func pngBytes() []byte { return []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a} }

func TestChatCompileInlineImage(t *testing.T) {
	source, err := media.NewImageBytes(pngBytes(), "image/png")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	request := mediaRequest(t,
		message.TextPart{Text: "what is this"},
		message.ImagePart{Source: source},
	)
	d := &driver{settings: testSettings(APIChat)}
	compiled, err := d.chatCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	messages, _ := wire["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %d", len(messages))
	}
	msg, _ := messages[0].(map[string]any)
	content, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content is not an array: %T", msg["content"])
	}
	if len(content) != 2 {
		t.Fatalf("content parts = %d", len(content))
	}
	text, _ := content[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "what is this" {
		t.Fatalf("text part = %v", text)
	}
	image, _ := content[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("image type = %v", image["type"])
	}
	imageURL, _ := image["image_url"].(map[string]any)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes())
	if imageURL["url"] != want {
		t.Fatalf("image url = %v", imageURL["url"])
	}
}

func TestChatCompileRemoteImage(t *testing.T) {
	source, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	request := mediaRequest(t, message.ImagePart{Source: source})
	d := &driver{settings: testSettings(APIChat)}
	compiled, err := d.chatCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	messages, _ := wire["messages"].([]any)
	msg, _ := messages[0].(map[string]any)
	content, _ := msg["content"].([]any)
	image, _ := content[0].(map[string]any)
	imageURL, _ := image["image_url"].(map[string]any)
	if imageURL["url"] != "https://example.com/cat.png" {
		t.Fatalf("image url = %v", imageURL["url"])
	}
}

func TestChatCompileInlineAudio(t *testing.T) {
	samples := []byte("RIFFfakewave")
	source, err := media.NewAudioBytes(samples, "audio/wav")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	request := mediaRequest(t, message.TextPart{Text: "transcribe"}, message.AudioPart{Source: source})
	d := &driver{settings: testSettings(APIChat)}
	compiled, err := d.chatCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	messages, _ := wire["messages"].([]any)
	msg, _ := messages[0].(map[string]any)
	content, _ := msg["content"].([]any)
	audio, _ := content[1].(map[string]any)
	if audio["type"] != "input_audio" {
		t.Fatalf("audio type = %v", audio["type"])
	}
	payload, _ := audio["input_audio"].(map[string]any)
	if payload["format"] != "wav" {
		t.Fatalf("format = %v", payload["format"])
	}
	if payload["data"] != base64.StdEncoding.EncodeToString(samples) {
		t.Fatalf("data = %v", payload["data"])
	}
}

func TestChatCompileRejectsUnsupportedAudioFormat(t *testing.T) {
	source, err := media.NewAudioBytes([]byte("zzzz"), "audio/aac")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	request := mediaRequest(t, message.AudioPart{Source: source})
	d := &driver{settings: testSettings(APIChat)}
	_, err = d.chatCompile(context.Background(), request, protocol.ShapeUnary)
	if !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("kind = %v, want UnsupportedFeature", err)
	}
}

// TestChatCompileTextOnlyStaysString guards the common path: a text-only turn
// must keep the plain string form every OpenAI-compatible server accepts.
func TestChatCompileTextOnlyStaysString(t *testing.T) {
	request := mediaRequest(t, message.TextPart{Text: "hello"})
	d := &driver{settings: testSettings(APIChat)}
	compiled, err := d.chatCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	messages, _ := wire["messages"].([]any)
	msg, _ := messages[0].(map[string]any)
	if _, ok := msg["content"].(string); !ok {
		t.Fatalf("content is not a string: %T", msg["content"])
	}
}

func TestResponsesCompileInlineImage(t *testing.T) {
	source, err := media.NewImageBytes(pngBytes(), "image/png")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	request := mediaRequest(t,
		message.TextPart{Text: "what is this"},
		message.ImagePart{Source: source},
	)
	d := &driver{settings: testSettings(APIResponses)}
	compiled, err := d.responsesCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	items, _ := wire["input"].([]any)
	if len(items) != 1 {
		t.Fatalf("input items = %d", len(items))
	}
	item, _ := items[0].(map[string]any)
	content, ok := item["content"].([]any)
	if !ok {
		t.Fatalf("content is not a list: %T", item["content"])
	}
	if len(content) != 2 {
		t.Fatalf("content parts = %d", len(content))
	}
	text, _ := content[0].(map[string]any)
	if text["type"] != "input_text" || text["text"] != "what is this" {
		t.Fatalf("text part = %v", text)
	}
	image, _ := content[1].(map[string]any)
	if image["type"] != "input_image" {
		t.Fatalf("image type = %v", image["type"])
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes())
	if image["image_url"] != want {
		t.Fatalf("image url = %v", image["image_url"])
	}
	if image["detail"] != "auto" {
		t.Fatalf("detail = %v", image["detail"])
	}
}

// TestResponsesRejectsAudio records a real gap instead of dropping the part: the
// Responses input item union in this SDK version has no audio variant.
func TestResponsesRejectsAudio(t *testing.T) {
	source, err := media.NewAudioBytes([]byte("RIFF"), "audio/wav")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	request := mediaRequest(t, message.AudioPart{Source: source})
	d := &driver{settings: testSettings(APIResponses)}
	_, err = d.responsesCompile(context.Background(), request, protocol.ShapeUnary)
	if !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("kind = %v, want UnsupportedFeature", err)
	}
}

// TestLiveImageInput is the credentialed lane for media input: a 1x1 red PNG is
// sent inline and the model is asked to name its colour.
func TestLiveImageInput(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	const redPixel = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	data, err := base64.StdEncoding.DecodeString(redPixel)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	source, err := media.NewImageBytes(data, "image/png")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	for _, api := range []API{APIChat, APIResponses} {
		t.Run(string(api), func(t *testing.T) {
			provider, err := New(Settings{API: api, APIKey: key})
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			request := inference.GenerateRequest{
				Model: model.ModelRef{
					ID:      model.ModelID{Provider: ProviderID, Name: "gpt-4o-mini"},
					Profile: "default",
				},
				Input: inference.GenerateInput{
					Role: inference.InputRoleUser,
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "What colour is this 1x1 image? Answer with one word."},
						message.ImagePart{Source: source},
					}},
				},
			}
			response, err := provider.Generate(context.Background(), request)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			t.Logf("%s -> %q", api, response.Message.Content.Text())
			if response.Message.Content.Text() == "" {
				t.Fatal("empty answer")
			}
		})
	}
}
