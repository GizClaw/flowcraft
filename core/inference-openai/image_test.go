package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// redPixelPNG is a 1x1 red PNG, used as the payload an image endpoint would
// return.
const redPixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func imageRequest(t *testing.T, prompt string) inference.GenerateRequest {
	t.Helper()
	request := inference.GenerateRequest{
		Model: model.ModelRef{
			ID:      model.ModelID{Provider: ProviderID, Name: "gpt-image-1"},
			Profile: "default",
		},
		Output: inference.OutputImage,
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent(prompt),
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

func TestImageCompileProducesWire(t *testing.T) {
	count := 2
	request := imageRequest(t, "a red square")
	request.Extensions = inference.Extensions{&ImageOptions{
		Count:   &count,
		Size:    "1024x1024",
		Quality: "high",
	}}
	d := &driver{settings: testSettings(APIChat)}
	compiled, err := d.imageCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	if wire["prompt"] != "a red square" {
		t.Fatalf("prompt = %v", wire["prompt"])
	}
	if wire["model"] != "gpt-image-1" {
		t.Fatalf("model = %v", wire["model"])
	}
	if wire["n"] != float64(2) {
		t.Fatalf("n = %v", wire["n"])
	}
	if wire["size"] != "1024x1024" || wire["quality"] != "high" {
		t.Fatalf("size/quality = %v/%v", wire["size"], wire["quality"])
	}
	want := inference.Field("extension.openai.image.size")
	found := false
	for _, field := range compiled.Fields {
		if field == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("option field %q not reported: %v", want, compiled.Fields)
	}
}

func TestImageCompileRejectsContext(t *testing.T) {
	request := imageRequest(t, "a red square")
	request.Context = []message.Message{message.NewTextMessage(message.RoleUser, "earlier")}
	d := &driver{settings: testSettings(APIChat)}
	_, err := d.imageCompile(context.Background(), request, protocol.ShapeUnary)
	if !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("kind = %v, want UnsupportedFeature", err)
	}
}

func TestImageCompileRejectsNonTextInput(t *testing.T) {
	source, err := media.NewImageBytes([]byte("png"), "image/png")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	request := imageRequest(t, "make it blue")
	request.Input.Content.Parts = append(request.Input.Content.Parts, message.ImagePart{Source: source})
	d := &driver{settings: testSettings(APIChat)}
	_, err = d.imageCompile(context.Background(), request, protocol.ShapeUnary)
	if !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("kind = %v, want UnsupportedFeature", err)
	}
}

func TestImageCompileRequiresPrompt(t *testing.T) {
	request := imageRequest(t, "   ")
	d := &driver{settings: testSettings(APIChat)}
	_, err := d.imageCompile(context.Background(), request, protocol.ShapeUnary)
	if !inference.IsKind(err, inference.InvalidRequest) {
		t.Fatalf("kind = %v, want InvalidRequest", err)
	}
}

func TestImageDecodeInlineBytesAndRevisedPrompt(t *testing.T) {
	request := imageRequest(t, "a red square")
	request.Extensions = inference.Extensions{&ImageOptions{OutputFormat: "png"}}
	raw := openaiImagesResponse(t, `{
		"created": 0,
		"data": [{"b64_json": "`+redPixelPNG+`", "revised_prompt": "a crimson square"}],
		"usage": {"input_tokens": 10, "output_tokens": 100, "total_tokens": 110,
			"input_tokens_details": {"text_tokens": 10, "image_tokens": 0},
			"output_tokens_details": {"image_tokens": 100, "text_tokens": 0}}
	}`)
	d := &driver{settings: testSettings(APIChat)}
	response, err := d.imageDecode(context.Background(), request, raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("response invalid: %v", err)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want image plus data", len(parts))
	}
	image, ok := parts[0].(message.ImagePart)
	if !ok {
		t.Fatalf("part 0 = %T", parts[0])
	}
	if image.Source.MediaType() != "image/png" {
		t.Fatalf("media type = %q", image.Source.MediaType())
	}
	if len(image.Source.Bytes()) == 0 {
		t.Fatal("image bytes are empty")
	}
	data, ok := parts[1].(message.DataPart)
	if !ok {
		t.Fatalf("part 1 = %T, want message.DataPart", parts[1])
	}
	var decoded struct {
		RevisedPrompts []struct {
			Index  int    `json:"index"`
			Prompt string `json:"prompt"`
		} `json:"revised_prompts"`
	}
	if err := json.Unmarshal(data.Value, &decoded); err != nil {
		t.Fatalf("data value: %v", err)
	}
	if len(decoded.RevisedPrompts) != 1 ||
		decoded.RevisedPrompts[0].Index != 0 ||
		decoded.RevisedPrompts[0].Prompt != "a crimson square" {
		t.Fatalf("revised prompts = %+v", decoded.RevisedPrompts)
	}
	if response.Usage.GeneratedImages == nil || *response.Usage.GeneratedImages != 1 {
		t.Fatalf("generated images = %v", response.Usage.GeneratedImages)
	}
	if response.Metadata.Model.Name != "gpt-image-1" {
		t.Fatalf("model = %q", response.Metadata.Model.Name)
	}
}

func TestImageDecodeURLWarnsAboutExpiry(t *testing.T) {
	request := imageRequest(t, "a red square")
	raw := openaiImagesResponse(t, `{
		"created": 0,
		"data": [{"url": "https://example.com/a.png", "revised_prompt": ""}],
		"usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
			"input_tokens_details": {"text_tokens": 1, "image_tokens": 0}}
	}`)
	d := &driver{settings: testSettings(APIChat)}
	response, err := d.imageDecode(context.Background(), request, raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d, want only the image", len(response.Message.Content.Parts))
	}
	if len(response.Warnings) != 1 || response.Warnings[0].Code != "ephemeral_url" {
		t.Fatalf("warnings = %+v", response.Warnings)
	}
}

func TestImageOptionsDecodeFromConfig(t *testing.T) {
	entries := []inference.ExtensionEntry{{
		Provider: ProviderID,
		ID:       ImageExtensionID,
		Fields:   json.RawMessage(`{"count": 2, "size": "1024x1024", "response_format": "b64_json"}`),
	}}
	extensions, err := inference.DecodeExtensions(entries, Decoders(), "node extensions")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(extensions) != 1 {
		t.Fatalf("extensions = %d", len(extensions))
	}
	if _, ok := extensions[0].(*ImageOptions); !ok {
		t.Fatalf("decoded %T", extensions[0])
	}
}

// TestStreamDispatchesByOutput pins that streaming picks a pipeline from the
// request: image output without a reference image goes to the generation stream,
// with one it goes to the edit stream, and text goes to the text stream.
func TestStreamDispatchesByOutput(t *testing.T) {
	probe := func(d *driver, request inference.GenerateRequest, want string) {
		t.Helper()
		generation := &recordingStream{}
		edit := &recordingStream{}
		text := &recordingStream{}
		d.imageStream = generation
		d.imageEditStream = edit
		d.textStream = text
		if _, err := d.Open(context.Background(), request); err == nil {
			t.Fatalf("%s: stub stream did not report its use", want)
		}
		calls := map[string]bool{
			"generation": generation.called,
			"edit":       edit.called,
			"text":       text.called,
		}
		for name, called := range calls {
			if (name == want) != called {
				t.Fatalf("%s: dispatch = %v", want, calls)
			}
		}
	}

	d := &driver{settings: testSettings(APIChat)}
	probe(d, imageRequest(t, "a red square"), "generation")
	probe(d, editRequest(t, "make it blue", []byte("IMG")), "edit")

	textRequest := imageRequest(t, "hello")
	textRequest.Output = inference.OutputText
	probe(d, textRequest, "text")
}

// recordingStream records that it was reached and then fails, so the test never
// needs a real provider.
type recordingStream struct{ called bool }

func (s *recordingStream) Open(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	s.called = true
	return nil, errors.New("recording stub")
}

// TestProviderDispatchesByOutput walks the whole path against a fake endpoint.
func TestProviderDispatchesByOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/generations" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var received map[string]any
		_ = json.Unmarshal(body, &received)
		if received["prompt"] != "a red square" {
			t.Errorf("prompt = %v", received["prompt"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":0,"data":[{"b64_json":"` + redPixelPNG + `"}],
			"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,
			"input_tokens_details":{"text_tokens":1,"image_tokens":0}}}`))
	}))
	defer server.Close()

	provider, err := New(Settings{API: APIChat, APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	response, err := provider.Generate(context.Background(), imageRequest(t, "a red square"))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	parts := response.Message.Content.Parts
	if len(parts) == 0 {
		t.Fatal("no parts")
	}
	if _, ok := parts[0].(message.ImagePart); !ok {
		t.Fatalf("part 0 = %T, want message.ImagePart", parts[0])
	}
}

// TestLiveImageGeneration is the credentialed lane: it costs money, so it is gated
// and skipped unless a key is present.
func TestLiveImageGeneration(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	provider, err := New(Settings{API: APIChat, APIKey: key})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	request := inference.GenerateRequest{
		Model: model.ModelRef{
			ID:      model.ModelID{Provider: ProviderID, Name: "gpt-image-1-mini"},
			Profile: "default",
		},
		Output: inference.OutputImage,
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("a single red square, flat colour"),
		},
	}
	count := 1
	request.Extensions = inference.Extensions{&ImageOptions{Count: &count, Size: "1024x1024"}}
	response, err := provider.Generate(context.Background(), request)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	t.Logf("images=%d usage=%+v", len(response.Message.Content.Parts), response.Usage)
	if len(response.Message.Content.Parts) == 0 {
		t.Fatal("no image returned")
	}
}

// openaiImagesResponse decodes a canned image response.
func openaiImagesResponse(t *testing.T, body string) *openai.ImagesResponse {
	t.Helper()
	var response openai.ImagesResponse
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("decode canned response: %v", err)
	}
	return &response
}
