package openai

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/goccy/go-json"
)

func editRequest(t *testing.T, prompt string, images ...[]byte) inference.GenerateRequest {
	t.Helper()
	parts := []message.Part{message.TextPart{Text: prompt}}
	for _, data := range images {
		source, err := media.NewImageBytes(data, "image/png")
		if err != nil {
			t.Fatalf("source: %v", err)
		}
		parts = append(parts, message.ImagePart{Source: source})
	}
	request := imageRequest(t, prompt)
	request.Input.Content = message.Content{Parts: parts}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

func TestImageEditCompileSingleImage(t *testing.T) {
	request := editRequest(t, "make it blue", []byte("PNGDATA"))
	d := &driver{settings: testSettings(APIChat)}
	compiled, err := d.imageEditCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	if wire["prompt"] != "make it blue" {
		t.Fatalf("prompt = %v", wire["prompt"])
	}
	if compiled.Wire.Image.OfFile == nil {
		t.Fatal("single image was not attached")
	}
	data, err := io.ReadAll(compiled.Wire.Image.OfFile)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	if string(data) != "PNGDATA" {
		t.Fatalf("image bytes = %q", data)
	}
}

func TestImageEditCompileMultipleImages(t *testing.T) {
	request := editRequest(t, "combine", []byte("ONE"), []byte("TWO"))
	d := &driver{settings: testSettings(APIChat)}
	compiled, err := d.imageEditCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(compiled.Wire.Image.OfFileArray) != 2 {
		t.Fatalf("images = %d, want 2", len(compiled.Wire.Image.OfFileArray))
	}
	first, _ := io.ReadAll(compiled.Wire.Image.OfFileArray[0])
	second, _ := io.ReadAll(compiled.Wire.Image.OfFileArray[1])
	if string(first) != "ONE" || string(second) != "TWO" {
		t.Fatalf("image order = %q, %q", first, second)
	}
}

func TestImageEditCompileMask(t *testing.T) {
	mask, err := media.NewImageBytes([]byte("MASK"), "image/png")
	if err != nil {
		t.Fatalf("mask: %v", err)
	}
	request := editRequest(t, "repaint", []byte("IMG"))
	request.Extensions = inference.Extensions{&ImageOptions{Mask: &mask}}
	d := &driver{settings: testSettings(APIChat)}
	compiled, err := d.imageEditCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.Mask == nil {
		t.Fatal("mask was not attached")
	}
	data, _ := io.ReadAll(compiled.Wire.Mask)
	if string(data) != "MASK" {
		t.Fatalf("mask bytes = %q", data)
	}
}

// TestImageEditRejectsURLSource keeps compilation I/O-free: a remote reference
// would have to be fetched, so it is refused instead of silently skipped.
func TestImageEditRejectsURLSource(t *testing.T) {
	source, err := media.NewImageURL("https://example.com/a.png", "image/png")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	request := imageRequest(t, "make it blue")
	request.Input.Content.Parts = []message.Part{
		message.TextPart{Text: "make it blue"},
		message.ImagePart{Source: source},
	}
	d := &driver{settings: testSettings(APIChat)}
	_, err = d.imageEditCompile(context.Background(), request, protocol.ShapeUnary)
	if !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("kind = %v, want UnsupportedFeature", err)
	}
}

func TestImageEditRequiresImageAndPrompt(t *testing.T) {
	d := &driver{settings: testSettings(APIChat)}

	noImage := imageRequest(t, "make it blue")
	if _, err := d.imageEditCompile(context.Background(), noImage, protocol.ShapeUnary); !inference.IsKind(err, inference.InvalidRequest) {
		t.Fatalf("missing image: kind = %v", err)
	}

	noPrompt := editRequest(t, "   ", []byte("IMG"))
	if _, err := d.imageEditCompile(context.Background(), noPrompt, protocol.ShapeUnary); !inference.IsKind(err, inference.InvalidRequest) {
		t.Fatalf("missing prompt: kind = %v", err)
	}
}

func TestImageEditRejectsContext(t *testing.T) {
	request := editRequest(t, "make it blue", []byte("IMG"))
	request.Context = []message.Message{message.NewTextMessage(message.RoleUser, "earlier")}
	d := &driver{settings: testSettings(APIChat)}
	if _, err := d.imageEditCompile(context.Background(), request, protocol.ShapeUnary); !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("kind = %v, want UnsupportedFeature", err)
	}
}

// TestProviderDispatchesToEdit proves the surface is chosen by content: the same
// output with a reference image reaches /images/edits.
func TestProviderDispatchesToEdit(t *testing.T) {
	var sawMultipart bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/edits" {
			t.Errorf("path = %q, want /images/edits", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		sawMultipart = r.MultipartForm != nil
		if got := r.FormValue("prompt"); got != "make it blue" {
			t.Errorf("prompt = %q", got)
		}
		if files := r.MultipartForm.File["image"]; len(files) != 1 {
			t.Errorf("image files = %d", len(files))
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
	response, err := provider.Generate(context.Background(), editRequest(t, "make it blue", []byte("IMG")))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !sawMultipart {
		t.Fatal("request was not multipart")
	}
	if len(response.Message.Content.Parts) == 0 {
		t.Fatal("no image returned")
	}
	if _, ok := response.Message.Content.Parts[0].(message.ImagePart); !ok {
		t.Fatalf("part = %T", response.Message.Content.Parts[0])
	}
}

// TestImageStreamPreviewReplacedByFinal pins the assembler contract: previews
// share index zero, so the finished image is what Result reports while the caller
// still observes every preview.
func TestImageStreamPreviewReplacedByFinal(t *testing.T) {
	preview := base64.StdEncoding.EncodeToString([]byte("PREVIEW"))
	final := base64.StdEncoding.EncodeToString([]byte("FINAL"))
	raw := &fakeImageStream{chunks: []string{
		`{"type":"image_generation.partial_image","b64_json":"` + preview + `","partial_image_index":0,"created_at":0,"size":"1024x1024","quality":"low","background":"auto","output_format":"png"}`,
		`{"type":"image_generation.completed","b64_json":"` + final + `","created_at":0,"size":"1024x1024","quality":"low","background":"auto","output_format":"png","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5,"input_tokens_details":{"text_tokens":2,"image_tokens":0}}}`,
	}}

	driver, err := protocol.BindGenerateStream(
		ProviderID,
		func(_ context.Context, r inference.GenerateRequest, _ protocol.Shape) (protocol.Compiled[openai.ImageGenerateParams], error) {
			return protocol.Compiled[openai.ImageGenerateParams]{
				Wire:   openai.ImageGenerateParams{},
				Fields: r.ActiveFields(),
			}, nil
		},
		func(context.Context, inference.GenerateRequest, openai.ImageGenerateParams) (protocol.RawStream[openai.ImageGenStreamEventUnion], error) {
			return raw, nil
		},
		imageStreamDecoder,
	)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	stream, err := driver.Open(context.Background(), imageRequest(t, "a red square"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var kinds []inference.StreamEventKind
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		kinds = append(kinds, event.Kind)
	}
	want := []inference.StreamEventKind{
		inference.StreamStart, inference.StreamPart,
		inference.StreamPart, inference.StreamUsage, inference.StreamEnd,
	}
	if len(kinds) != len(want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event %d = %v, want %v", i, kinds[i], want[i])
		}
	}

	response, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	part, ok := response.Message.Content.Parts[0].(message.ImagePart)
	if !ok {
		t.Fatalf("part = %T", response.Message.Content.Parts[0])
	}
	if string(part.Source.Bytes()) != "FINAL" {
		t.Fatalf("assembled image = %q, want the finished one", part.Source.Bytes())
	}
	if got := response.Usage.WorkValue(usage.WorkImagesGenerated); got != 1 {
		t.Fatalf("generated images = %d, want 1", got)
	}
}

func TestImageStreamRejectsMultipleImages(t *testing.T) {
	count := 2
	request := imageRequest(t, "two squares")
	request.Extensions = inference.Extensions{&ImageOptions{Count: &count}}
	d := &driver{settings: testSettings(APIChat)}
	if _, err := d.imageCompile(context.Background(), request, protocol.ShapeStream); !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("kind = %v, want UnsupportedFeature", err)
	}
}

// fakeImageStream replays canned image events at the wire level.
type fakeImageStream struct {
	chunks []string
	index  int
}

func (s *fakeImageStream) Next(context.Context) (openai.ImageGenStreamEventUnion, error) {
	if s.index >= len(s.chunks) {
		return openai.ImageGenStreamEventUnion{}, io.EOF
	}
	var event openai.ImageGenStreamEventUnion
	if err := json.Unmarshal([]byte(s.chunks[s.index]), &event); err != nil {
		return event, err
	}
	s.index++
	return event, nil
}

func (s *fakeImageStream) Close() error { return nil }

// TestLiveImageEdit is the credentialed lane for the edit surface.
func TestLiveImageEdit(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	data, err := base64.StdEncoding.DecodeString(redPixelPNG)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	provider, err := New(Settings{API: APIChat, APIKey: key})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	request := editRequest(t, "make this square blue", data)
	request.Model.ID.Name = "gpt-image-1-mini"
	response, err := provider.Generate(context.Background(), request)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	parts := response.Message.Content.Parts
	t.Logf("parts=%d generated=%d", len(parts), response.Usage.WorkValue(usage.WorkImagesGenerated))
	if len(parts) == 0 {
		t.Fatal("no image returned")
	}
	if _, ok := parts[0].(message.ImagePart); !ok {
		t.Fatalf("part 0 = %T", parts[0])
	}
}

// TestLiveImageStream is the credentialed lane for streamed generation.
func TestLiveImageStream(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	provider, err := New(Settings{API: APIChat, APIKey: key})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	partials := 1
	request := imageRequest(t, "a single blue square, flat colour")
	request.Model.ID.Name = "gpt-image-1-mini"
	request.Extensions = inference.Extensions{&ImageOptions{PartialImages: &partials}}

	stream, err := provider.GenerateStream(context.Background(), request)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	parts := 0
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if event.Kind == inference.StreamPart {
			parts++
		}
	}
	response, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	t.Logf("part events=%d final parts=%d", parts, len(response.Message.Content.Parts))
	if parts < 2 {
		t.Fatalf("expected at least one preview plus the final image, got %d", parts)
	}
}

// TestImageEditRejectsJSONSet pins the escape hatch's boundary: the edits surface
// is multipart, so a JSON-only injection cannot reach it.
func TestImageEditRejectsJSONSet(t *testing.T) {
	request := editRequest(t, "make it blue", []byte("IMG"))
	request.Extensions = inference.Extensions{&GenerationOptions{
		JSONSet: map[string]json.RawMessage{"area": json.RawMessage(`"2K"`)},
	}}
	d := &driver{settings: testSettings(APIChat)}
	_, err := d.imageEditCompile(context.Background(), request, protocol.ShapeUnary)
	if !inference.IsKind(err, inference.InvalidExtension) {
		t.Fatalf("kind = %v, want InvalidExtension", err)
	}
}
