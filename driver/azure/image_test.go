package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/openai/openai-go/v3"
)

// ---------------------------------------------------------------------------
// Shared fixtures.
// ---------------------------------------------------------------------------

// capturedAzure serves the API surface used by the driver and records every
// request body for assertion.
type capturedAzure struct {
	t *testing.T

	bodies  []map[string]any
	handler func(w http.ResponseWriter, r *http.Request, body map[string]any)
}

func newCapturedAzure(
	t *testing.T,
	handler func(w http.ResponseWriter, r *http.Request, body map[string]any),
) (*httptest.Server, *capturedAzure) {
	capture := &capturedAzure{t: t, handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		// Restore the body so handlers can re-read multipart forms.
		r.Body = io.NopCloser(bytes.NewReader(payload))
		var body map[string]any
		if len(payload) > 0 &&
			strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Errorf("body is not JSON: %v", err)
				return
			}
		}
		capture.bodies = append(capture.bodies, body)
		handler(w, r, body)
	}))
	return server, capture
}

func (c *capturedAzure) body(index int) map[string]any {
	c.t.Helper()
	if index >= len(c.bodies) {
		c.t.Fatalf("only %d captured requests", len(c.bodies))
	}
	return c.bodies[index]
}

func azureImageClients(t *testing.T, server *httptest.Server) *clients {
	t.Helper()
	spec, err := decodeSpec([]byte(
		fmt.Sprintf(`{
			"endpoint": %q,
			"models": [{"name": "gpt-image-1", "kind": "image",
				"capabilities": {"outputs": ["image"]}}]
		}`, server.URL),
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	return profileMaterial{apiKey: "test-key"}.newClients(spec)
}

func azureImageModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: driverID, Name: name},
		Profile: "default",
	}
}

// testPNG is a 1x1 transparent PNG used as reference image and mask.
var testPNG = func() []byte {
	data, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
	)
	if err != nil {
		panic(err)
	}
	return data
}()

func imageResponse(w http.ResponseWriter, inputTokens int64) {
	w.Header().Set("Content-Type", "application/json")
	payload, _ := json.Marshal(map[string]any{
		"data":  []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(testPNG)}},
		"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
	})
	_, _ = fmt.Fprint(w, string(payload))
}

// ---------------------------------------------------------------------------
// Transport.
// ---------------------------------------------------------------------------

func TestImageTransport(t *testing.T) {
	server, capture := newCapturedAzure(t, func(
		w http.ResponseWriter,
		_ *http.Request,
		body map[string]any,
	) {
		if size, _ := body["size"].(string); size != "1728x2304" {
			t.Errorf("size = %v", body["size"])
		}
		if fmt.Sprint(body["output_format"]) != "png" {
			t.Errorf("output_format = %v", body["output_format"])
		}
		if quality, _ := body["quality"].(string); quality != "high" {
			t.Errorf("quality = %v, want high", body["quality"])
		}
		w.Header().Set("x-request-id", "req_azure_1")
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"id":    "img_resp_1",
			"data":  []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(testPNG)}},
			"usage": map[string]any{"input_tokens": 11, "output_tokens": 0},
		})
		_, _ = fmt.Fprint(w, string(payload))
	})
	defer server.Close()
	cls := azureImageClients(t, server)

	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "a red circle"},
				}},
				Intent: inference.Intent{Image: &inference.ImageIntent{
					Size:         &media.ImageSize{Width: 1728, Height: 2304},
					OutputFormat: media.ImageFormatPNG,
					Quality:      media.ImageQualityHigh,
				}},
			},
		},
	}
	compiled, err := compileImage("gpt-image-1")(
		context.Background(),
		azureImageModel("gpt-image-1"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileImage: %v", err)
	}
	raw, err := transportImage(cls.api)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("transportImage: %v", err)
	}
	response, err := decodeImage(context.Background(), raw)
	if err != nil {
		t.Fatalf("decodeImage: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	part, ok := response.Message.Content.Parts[0].(message.ImagePart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if part.Source.Kind() != media.SourceInline ||
		!bytes.Equal(part.Source.Bytes(), testPNG) {
		t.Fatalf("image source bytes mismatch")
	}
	if response.Metadata.RequestID != "req_azure_1" {
		t.Errorf("metadata request_id = %q, want req_azure_1",
			response.Metadata.RequestID)
	}
	if response.Metadata.ResponseID != "img_resp_1" {
		t.Errorf("metadata response_id = %q, want img_resp_1",
			response.Metadata.ResponseID)
	}
	_ = capture.body(0)
}

// ---------------------------------------------------------------------------
// Streaming.
// ---------------------------------------------------------------------------

func imageGenPartialEvent(b64 string, index int) string {
	return fmt.Sprintf(
		`{"type":"image_generation.partial_image","b64_json":%q,`+
			`"output_format":"png","partial_image_index":%d,`+
			`"created_at":1,"size":"1024x1024","quality":"high","background":"auto"}`,
		b64,
		index,
	)
}

func imageGenCompletedEvent(
	b64 string,
	inputTokens, outputTokens, totalTokens int64,
) string {
	return fmt.Sprintf(
		`{"type":"image_generation.completed","b64_json":%q,`+
			`"output_format":"png","created_at":2,"size":"1024x1024",`+
			`"quality":"high","background":"auto",`+
			`"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}`,
		b64,
		inputTokens,
		outputTokens,
		totalTokens,
	)
}

func imageEditPartialEvent(b64 string, index int) string {
	return fmt.Sprintf(
		`{"type":"image_edit.partial_image","b64_json":%q,`+
			`"output_format":"png","partial_image_index":%d,`+
			`"created_at":1,"size":"1024x1024","quality":"high","background":"auto"}`,
		b64,
		index,
	)
}

func imageEditCompletedEvent(b64 string) string {
	return fmt.Sprintf(
		`{"type":"image_edit.completed","b64_json":%q,`+
			`"output_format":"png","created_at":2,"size":"1024x1024",`+
			`"quality":"high","background":"auto",`+
			`"usage":{"input_tokens":4,"output_tokens":9,"total_tokens":13}}`,
		b64,
	)
}

func writeImageSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
	}
}

func TestImageStreamTransport(t *testing.T) {
	server, capture := newCapturedAzure(t, func(
		w http.ResponseWriter,
		_ *http.Request,
		body map[string]any,
	) {
		if stream, _ := body["stream"].(bool); !stream {
			t.Errorf("stream = %v, want true", body["stream"])
		}
		if partial, _ := body["partial_images"].(float64); partial != 2 {
			t.Errorf("partial_images = %v, want 2", body["partial_images"])
		}
		if prompt, _ := body["prompt"].(string); prompt != "a red circle" {
			t.Errorf("prompt = %v", body["prompt"])
		}
		w.Header().Set("x-request-id", "req_azure_stream_1")
		b64 := base64.StdEncoding.EncodeToString(testPNG)
		writeImageSSE(
			w,
			imageGenPartialEvent(b64, 0),
			imageGenPartialEvent(b64, 1),
			imageGenCompletedEvent(b64, 7, 13, 20),
			imageGenCompletedEvent(b64, 7, 13, 20),
		)
	})
	defer server.Close()
	cls := azureImageClients(t, server)

	two := 2
	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "a red circle"},
				}},
				Intent: inference.Intent{Image: &inference.ImageIntent{}},
			},
		},
		Extensions: inference.Extensions{
			ImageOptions{PartialImages: &two},
		},
	}
	compiled, err := compileImage("gpt-image-1")(
		context.Background(),
		azureImageModel("gpt-image-1"),
		request,
		inference.GenerateExecutionStream,
	)
	if err != nil {
		t.Fatalf("compileImage stream: %v", err)
	}
	if compiled.Wire.partialImages != 2 {
		t.Fatalf("wire partial_images = %d, want 2", compiled.Wire.partialImages)
	}

	rawStream, err := transportImageStream(cls.api)(
		context.Background(),
		compiled.Wire,
	)
	if err != nil {
		t.Fatalf("transportImageStream: %v", err)
	}
	var events []inference.GenerateStreamEvent
	for {
		raw, err := rawStream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		event, err := decodeImageStream(context.Background(), raw)
		if err != nil {
			t.Fatalf("decodeImageStream: %v", err)
		}
		events = append(events, event)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	assertImageStreamEvent := func(
		index int,
		partIndex int,
		interim bool,
		finish inference.FinishReason,
	) {
		t.Helper()
		event := events[index]
		if event.PartIndex != partIndex {
			t.Errorf("event %d part index = %d, want %d", index, event.PartIndex, partIndex)
		}
		delta, ok := event.Delta.(inference.ImagePartDelta)
		if !ok {
			t.Errorf("event %d delta = %T, want ImagePartDelta", index, event.Delta)
			return
		}
		if delta.Interim != interim {
			t.Errorf("event %d interim = %v, want %v", index, delta.Interim, interim)
		}
		if !bytes.Equal(delta.Part.Source.Bytes(), testPNG) {
			t.Errorf("event %d image bytes mismatch", index)
		}
		if event.FinishReason != finish {
			t.Errorf("event %d finish = %q, want %q", index, event.FinishReason, finish)
		}
	}
	assertImageStreamEvent(0, 0, true, "")
	assertImageStreamEvent(1, 0, true, "")
	assertImageStreamEvent(2, 0, false, inference.FinishCompleted)
	assertImageStreamEvent(3, 1, false, inference.FinishCompleted)

	if usage := events[2].Usage; usage == nil ||
		usage.InputTokens != 7 || usage.OutputTokens != 13 || usage.TotalTokens != 20 {
		t.Fatalf("completed usage = %+v, want 7/13/20", events[2].Usage)
	}
	if events[0].Usage != nil {
		t.Fatalf("partial event carried usage %+v, want none", events[0].Usage)
	}
	for _, index := range []int{0, 1} {
		if events[index].RequestID != "" {
			t.Errorf("partial event %d request_id = %q, want empty",
				index, events[index].RequestID)
		}
	}
	for _, index := range []int{2, 3} {
		if events[index].RequestID != "req_azure_stream_1" {
			t.Errorf("completed event %d request_id = %q, want req_azure_stream_1",
				index, events[index].RequestID)
		}
	}
	_ = capture.body(0)
}

func TestImageEditStreamTransport(t *testing.T) {
	source, err := media.NewImageBytes(testPNG, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	server, capture := newCapturedAzure(t, func(
		w http.ResponseWriter,
		r *http.Request,
		_ map[string]any,
	) {
		if r.URL.Path != "/openai/deployments/gpt-image-1/images/edits" {
			t.Errorf("path = %s, want /openai/deployments/gpt-image-1/images/edits",
				r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		if stream := r.MultipartForm.Value["stream"]; len(stream) != 1 ||
			stream[0] != "true" {
			t.Errorf("stream form field = %v, want [true]", stream)
		}
		if partial := r.MultipartForm.Value["partial_images"]; len(partial) != 1 ||
			partial[0] != "2" {
			t.Errorf("partial_images form field = %v, want [2]", partial)
		}
		if prompt := r.MultipartForm.Value["prompt"]; len(prompt) != 1 ||
			prompt[0] != "make it a red circle" {
			t.Errorf("prompt = %v", r.MultipartForm.Value["prompt"])
		}
		w.Header().Set("apim-request-id", "req_azure_edit_1")
		b64 := base64.StdEncoding.EncodeToString(testPNG)
		writeImageSSE(
			w,
			imageEditPartialEvent(b64, 0),
			imageEditCompletedEvent(b64),
		)
	})
	defer server.Close()
	cls := azureImageClients(t, server)

	two := 2
	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "make it a red circle"},
					message.ImagePart{Source: source},
				}},
				Intent: inference.Intent{Image: &inference.ImageIntent{}},
			},
		},
		Extensions: inference.Extensions{
			ImageOptions{PartialImages: &two},
		},
	}
	compiled, err := compileImage("gpt-image-1")(
		context.Background(),
		azureImageModel("gpt-image-1"),
		request,
		inference.GenerateExecutionStream,
	)
	if err != nil {
		t.Fatalf("compileImage stream: %v", err)
	}
	rawStream, err := transportImageStream(cls.api)(
		context.Background(),
		compiled.Wire,
	)
	if err != nil {
		t.Fatalf("transportImageStream: %v", err)
	}
	var events []inference.GenerateStreamEvent
	for {
		raw, err := rawStream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		event, err := decodeImageStream(context.Background(), raw)
		if err != nil {
			t.Fatalf("decodeImageStream: %v", err)
		}
		events = append(events, event)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].PartIndex != 0 ||
		events[0].Delta.(inference.ImagePartDelta).Interim != true {
		t.Fatalf("partial event = %+v, want part 0 interim", events[0])
	}
	if events[1].PartIndex != 0 ||
		events[1].Delta.(inference.ImagePartDelta).Interim != false ||
		events[1].FinishReason != inference.FinishCompleted {
		t.Fatalf("completed event = %+v, want part 0 final", events[1])
	}
	if events[1].RequestID != "req_azure_edit_1" {
		t.Errorf("completed request_id = %q, want req_azure_edit_1",
			events[1].RequestID)
	}
	_ = capture.body(0)
}

func TestImageCompilerStreamShape(t *testing.T) {
	compile := func(shape inference.GenerateExecutionShape, partial *int) (
		imageWire,
		error,
	) {
		t.Helper()
		request := inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "a red circle"},
					}},
					Intent: inference.Intent{Image: &inference.ImageIntent{}},
				},
			},
		}
		if partial != nil {
			request.Extensions = inference.Extensions{
				ImageOptions{PartialImages: partial},
			}
		}
		compiled, err := compileImage("gpt-image-1")(
			context.Background(),
			azureImageModel("gpt-image-1"),
			request,
			shape,
		)
		if err != nil {
			return imageWire{}, err
		}
		return compiled.Wire, nil
	}

	if _, err := compile(inference.GenerateExecutionStream, nil); err != nil {
		t.Fatalf("stream compile without extension: %v", err)
	}
	two := 2
	wire, err := compile(inference.GenerateExecutionStream, &two)
	if err != nil {
		t.Fatalf("stream compile with partial_images: %v", err)
	}
	if wire.partialImages != 2 {
		t.Fatalf("wire partial_images = %d, want 2", wire.partialImages)
	}

	// partial_images is stream-only: the unary shape rejects it.
	if _, err := compile(inference.GenerateExecutionUnary, &two); err == nil {
		t.Fatal("unary compile with partial_images succeeded, want rejection")
	} else {
		var inferenceErr *inference.Error
		if !errors.As(err, &inferenceErr) ||
			inferenceErr.Field != inference.ExtensionField("partial_images").Qualify(ImageOptions{}) {
			t.Fatalf("unary partial_images error = %v, want partial_images field", err)
		}
	}

	// Out-of-range preview counts are rejected at compile time.
	four := 4
	if _, err := compile(inference.GenerateExecutionStream, &four); err == nil {
		t.Fatal("partial_images=4 compile succeeded, want rejection")
	} else {
		var inferenceErr *inference.Error
		if !errors.As(err, &inferenceErr) ||
			inferenceErr.Field != inference.ExtensionField("partial_images").Qualify(ImageOptions{}) {
			t.Fatalf("partial_images=4 error = %v, want partial_images field", err)
		}
	}
}

func TestImageCompilerQuality(t *testing.T) {
	compile := func(quality media.ImageQuality) (string, error) {
		t.Helper()
		request := inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "a red circle"},
					}},
					Intent: inference.Intent{Image: &inference.ImageIntent{
						Quality: quality,
					}},
				},
			},
		}
		compiled, err := compileImage("gpt-image-1")(
			context.Background(),
			azureImageModel("gpt-image-1"),
			request,
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			return "", err
		}
		return compiled.Wire.quality, nil
	}

	for _, quality := range []media.ImageQuality{
		media.ImageQualityAuto,
		media.ImageQualityLow,
		media.ImageQualityMedium,
		media.ImageQualityHigh,
	} {
		got, err := compile(quality)
		if err != nil {
			t.Errorf("quality %q: compile = %v", quality, err)
			continue
		}
		if got != string(quality) {
			t.Errorf("quality %q: wire = %q", quality, got)
		}
	}

	_, err := compile(media.ImageQuality("ultra"))
	if err == nil {
		t.Fatal("unknown quality compiled, want rejection")
	}
	var inferenceErr *inference.Error
	if !errors.As(err, &inferenceErr) ||
		inferenceErr.Field != inference.FieldGenerateIntentImageQuality {
		t.Fatalf("unknown quality error = %v, want image quality field", err)
	}
}

func TestImageCompilerSizeRules(t *testing.T) {
	compile := func(width, height int) (string, error) {
		t.Helper()
		request := inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "a red circle"},
					}},
					Intent: inference.Intent{Image: &inference.ImageIntent{
						Size: &media.ImageSize{Width: width, Height: height},
					}},
				},
			},
		}
		compiled, err := compileImage("gpt-image-2")(
			context.Background(),
			azureImageModel("gpt-image-2"),
			request,
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			return "", err
		}
		return compiled.Wire.size, nil
	}

	for _, size := range []struct {
		width, height int
		want          string
	}{
		{1024, 1024, "1024x1024"},
		{1536, 1024, "1536x1024"},
		{1024, 1536, "1024x1536"},
		{1728, 2304, "1728x2304"},
		{864, 1072, "864x1072"},
		{2160, 3840, "2160x3840"},
	} {
		got, err := compile(size.width, size.height)
		if err != nil {
			t.Errorf("%dx%d: compile = %v, want size %q", size.width, size.height, err, size.want)
			continue
		}
		if got != size.want {
			t.Errorf("%dx%d: wire size = %q, want %q", size.width, size.height, got, size.want)
		}
	}

	for _, size := range []struct {
		width, height int
	}{
		{800, 600},   // not divisible by 16
		{1025, 1024}, // width not divisible by 16
		{4096, 512},  // aspect ratio 8:1
		{512, 4096},  // aspect ratio 1:8
		{3840, 3840}, // short edge above 2160
		{0, 1024},    // non-positive
		{-1, -1},     // non-positive
	} {
		_, err := compile(size.width, size.height)
		if err == nil {
			t.Errorf("%dx%d: compile succeeded, want size rejection", size.width, size.height)
			continue
		}
		var inferenceErr *inference.Error
		if !errors.As(err, &inferenceErr) ||
			inferenceErr.Field != inference.FieldGenerateIntentImageSize {
			t.Errorf("%dx%d: error = %v, want field %q",
				size.width, size.height, err, inference.FieldGenerateIntentImageSize)
		}
	}
}

// TestImageOpsBind guards the core binding contract: the image wire must
// stay concrete (no media sources, whose stream field is an interface), so
// opening image operations succeeds for both execution shapes.
func TestImageOpsBind(t *testing.T) {
	operations, err := inference.BindGenerateOperations(
		compileImage("gpt-image-1"),
		transportImage(openai.Client{}),
		decodeImage,
		transportImageStream(openai.Client{}),
		decodeImageStream,
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	if operations.Unary == nil || operations.Stream == nil {
		t.Fatal("BindGenerateOperations must bind unary and stream drivers")
	}
}

func TestImageEditTransport(t *testing.T) {
	source, err := media.NewImageBytes(testPNG, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	server, capture := newCapturedAzure(t, func(
		w http.ResponseWriter,
		r *http.Request,
		_ map[string]any,
	) {
		if r.URL.Path != "/openai/deployments/gpt-image-1/images/edits" {
			t.Errorf("path = %s, want /openai/deployments/gpt-image-1/images/edits",
				r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		files := r.MultipartForm.File["image[]"]
		if len(files) != 1 {
			t.Errorf("image files = %d, want 1", len(files))
			return
		}
		if contentType := files[0].Header.Get("Content-Type"); contentType != "image/png" {
			t.Errorf("image content type = %q, want image/png", contentType)
		}
		file, err := files[0].Open()
		if err != nil {
			t.Errorf("open image file: %v", err)
			return
		}
		defer func() { _ = file.Close() }()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Errorf("read image file: %v", err)
			return
		}
		if !bytes.Equal(data, testPNG) {
			t.Errorf("uploaded image bytes = %d, want %d", len(data), len(testPNG))
		}
		if prompt := r.MultipartForm.Value["prompt"]; len(prompt) != 1 ||
			prompt[0] != "make it a red circle" {
			t.Errorf("prompt = %v", r.MultipartForm.Value["prompt"])
		}
		imageResponse(w, 12)
	})
	defer server.Close()
	cls := azureImageClients(t, server)

	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "make it a red circle"},
					message.ImagePart{Source: source},
				}},
				Intent: inference.Intent{Image: &inference.ImageIntent{}},
			},
		},
	}
	compiled, err := compileImage("gpt-image-1")(
		context.Background(),
		azureImageModel("gpt-image-1"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileImage: %v", err)
	}
	if len(compiled.Wire.images) != 1 {
		t.Fatalf("wire images = %d, want 1", len(compiled.Wire.images))
	}
	raw, err := transportImage(cls.api)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("transportImage: %v", err)
	}
	if _, err := decodeImage(context.Background(), raw); err != nil {
		t.Fatalf("decodeImage: %v", err)
	}
	_ = capture.body(0)
}

func TestImageEditTransportMask(t *testing.T) {
	source, err := media.NewImageBytes(testPNG, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	mask, err := media.NewImageBytes(testPNG, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	server, capture := newCapturedAzure(t, func(
		w http.ResponseWriter,
		r *http.Request,
		_ map[string]any,
	) {
		if r.URL.Path != "/openai/deployments/gpt-image-1/images/edits" {
			t.Errorf("path = %s, want /openai/deployments/gpt-image-1/images/edits",
				r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		files := r.MultipartForm.File["image[]"]
		if len(files) != 1 {
			t.Errorf("image files = %d, want 1", len(files))
			return
		}
		masks := r.MultipartForm.File["mask"]
		if len(masks) != 1 {
			t.Errorf("mask files = %d, want 1", len(masks))
			return
		}
		if contentType := files[0].Header.Get("Content-Type"); contentType != "image/png" {
			t.Errorf("image content type = %q, want image/png", contentType)
		}
		if contentType := masks[0].Header.Get("Content-Type"); contentType != "image/png" {
			t.Errorf("mask content type = %q, want image/png", contentType)
		}
		readPart := func(header *multipart.FileHeader) []byte {
			t.Helper()
			file, err := header.Open()
			if err != nil {
				t.Errorf("open file: %v", err)
				return nil
			}
			defer func() { _ = file.Close() }()
			data, err := io.ReadAll(file)
			if err != nil {
				t.Errorf("read file: %v", err)
				return nil
			}
			return data
		}
		if data := readPart(files[0]); !bytes.Equal(data, testPNG) {
			t.Errorf("uploaded image bytes = %d, want %d", len(data), len(testPNG))
		}
		if data := readPart(masks[0]); !bytes.Equal(data, testPNG) {
			t.Errorf("uploaded mask bytes = %d, want %d", len(data), len(testPNG))
		}
		if size := r.MultipartForm.Value["size"]; len(size) != 1 ||
			size[0] != "1024x1024" {
			t.Errorf("size = %v", r.MultipartForm.Value["size"])
		}
		imageResponse(w, 13)
	})
	defer server.Close()
	cls := azureImageClients(t, server)

	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "replace the sky"},
					message.ImagePart{Source: source},
				}},
				Intent: inference.Intent{Image: &inference.ImageIntent{
					Size: &media.ImageSize{Width: 1024, Height: 1024},
				}},
			},
		},
		Extensions: inference.Extensions{
			ImageOptions{Mask: &mask},
		},
	}
	compiled, err := compileImage("gpt-image-1")(
		context.Background(),
		azureImageModel("gpt-image-1"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileImage: %v", err)
	}
	if len(compiled.Wire.images) != 1 {
		t.Fatalf("wire images = %d, want 1", len(compiled.Wire.images))
	}
	if !bytes.Equal(compiled.Wire.mask.data, testPNG) ||
		compiled.Wire.mask.mediaType != "image/png" {
		t.Fatalf("wire mask = %d bytes / %q, want %d bytes / image/png",
			len(compiled.Wire.mask.data), compiled.Wire.mask.mediaType, len(testPNG))
	}
	raw, err := transportImage(cls.api)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("transportImage: %v", err)
	}
	if _, err := decodeImage(context.Background(), raw); err != nil {
		t.Fatalf("decodeImage: %v", err)
	}
	_ = capture.body(0)
}
