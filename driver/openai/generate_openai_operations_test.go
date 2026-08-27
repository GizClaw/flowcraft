package openai

// Driver-level transport tests: each operation runs through the real SDK
// transport against a captured HTTP server, so request shaping and response
// decoding are both exercised.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/openai/openai-go/v3"
)

func TestGenerateUnaryToolCalls(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		if model, _ := body["model"].(string); model != "gpt-5.6-sol" {
			t.Errorf("request model = %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			toolCallOutputItem(),
		}))
	})
	defer server.Close()
	cls := testClients(t, server)
	operations, err := openGenerate(cls, catalog["gpt-5.6-sol"], openaiModel("gpt-5.6-sol").ID, "default")
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}
	request := simpleTextRequest("find something")
	request.Input.Content.Intent.Text.Tools = []message.ToolDefinition{{
		Name:        "lookup",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	response, err := operations.Unary.Execute(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = capture.body(0)
	if response.FinishReason != inference.FinishToolCalls {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	call, ok := response.Message.Content.Parts[0].(message.ToolCallPart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if call.Call.ID != "call_9" || call.Call.Name != "lookup" ||
		string(call.Call.Arguments) != `{"q":"openai"}` {
		t.Fatalf("call = %+v", call.Call)
	}
	if response.Usage.Input.CacheReadTokens == nil ||
		*response.Usage.Input.CacheReadTokens != 3 {
		t.Fatalf("cached usage = %+v", response.Usage.Input)
	}
	if response.Usage.Output.ReasoningTokens == nil ||
		*response.Usage.Output.ReasoningTokens != 2 {
		t.Fatalf("reasoning usage = %+v", response.Usage.Output)
	}
}

func TestEmbedTransport(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		input, _ := body["input"].([]any)
		if len(input) != 2 {
			t.Errorf("input = %v", body["input"])
		}
		if dimensions, _ := body["dimensions"].(float64); dimensions != 512 {
			t.Errorf("dimensions = %v", body["dimensions"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"object": "list",
			"data": [
				{"object": "embedding", "index": 0, "embedding": [0.5, -0.25]},
				{"object": "embedding", "index": 1, "embedding": [1.0, 2.0]}
			],
			"usage": {"prompt_tokens": 9, "total_tokens": 9}
		}`)
	})
	defer server.Close()
	cls := testClients(t, server)

	compiled, err := compileEmbed("text-embedding-3-large", catalog["text-embedding-3-large"])(
		context.Background(),
		openaiModel("text-embedding-3-large"),
		inference.EmbedRequest{
			Items: []inference.EmbedItem{
				{Content: message.Content{Parts: []message.Part{message.TextPart{Text: "a"}}}},
				{Content: message.Content{Parts: []message.Part{message.TextPart{Text: "b"}}}},
			},
			Dimensions: intPointer(512),
		},
	)
	if err != nil {
		t.Fatalf("compileEmbed: %v", err)
	}
	raw, err := transportEmbed(cls.api)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("transportEmbed: %v", err)
	}
	response, err := decodeEmbed(context.Background(), raw)
	if err != nil {
		t.Fatalf("decodeEmbed: %v", err)
	}
	if len(response.Embeddings) != 2 {
		t.Fatalf("embeddings = %d", len(response.Embeddings))
	}
	if response.Embeddings[0].Vector[0] != 0.5 ||
		response.Embeddings[1].Vector[1] != 2.0 {
		t.Fatalf("embeddings = %+v", response.Embeddings)
	}
	if response.Usage.InputTokens != 9 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	_ = capture.body(0)
}

func TestImageTransport(t *testing.T) {
	// 1x1 transparent PNG.
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
	)
	if err != nil {
		t.Fatal(err)
	}
	server, capture := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		if size, _ := body["size"].(string); size != "1728x2304" {
			t.Errorf("size = %v", body["size"])
		}
		if fmt.Sprint(body["output_format"]) != "png" {
			t.Errorf("output_format = %v", body["output_format"])
		}
		if quality, _ := body["quality"].(string); quality != "high" {
			t.Errorf("quality = %v, want high", body["quality"])
		}
		w.Header().Set("x-request-id", "req_openai_1")
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"id":    "img_resp_1",
			"data":  []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(png)}},
			"usage": map[string]any{"input_tokens": 11, "output_tokens": 0},
		})
		_, _ = fmt.Fprint(w, string(payload))
	})
	defer server.Close()
	cls := testClients(t, server)

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
	compiled, err := compileImage("gpt-image-2")(
		context.Background(),
		openaiModel("gpt-image-2"),
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
	if part.Source.Kind() != media.SourceInline || len(part.Source.Bytes()) != len(png) {
		t.Fatalf("image source = %v bytes", len(part.Source.Bytes()))
	}
	if response.Metadata.RequestID != "req_openai_1" {
		t.Errorf("metadata request_id = %q, want req_openai_1",
			response.Metadata.RequestID)
	}
	if response.Metadata.ResponseID != "img_resp_1" {
		t.Errorf("metadata response_id = %q, want img_resp_1",
			response.Metadata.ResponseID)
	}
	_ = capture.body(0)
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
			openaiModel("gpt-image-2"),
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
		compileImage("gpt-image-2"),
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
	// 1x1 transparent PNG reference image.
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
	)
	if err != nil {
		t.Fatal(err)
	}
	server, capture := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		r *http.Request,
		body map[string]any,
	) {
		if r.URL.Path != "/images/edits" {
			t.Errorf("path = %s, want /images/edits", r.URL.Path)
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
		if !bytes.Equal(data, png) {
			t.Errorf("uploaded image bytes = %d, want %d", len(data), len(png))
		}
		if prompt := r.MultipartForm.Value["prompt"]; len(prompt) != 1 ||
			prompt[0] != "make it a red circle" {
			t.Errorf("prompt = %v", r.MultipartForm.Value["prompt"])
		}
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"data":  []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(png)}},
			"usage": map[string]any{"input_tokens": 12, "output_tokens": 0},
		})
		_, _ = fmt.Fprint(w, string(payload))
	})
	defer server.Close()
	cls := testClients(t, server)

	source, err := media.NewImageBytes(png, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
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
	compiled, err := compileImage("gpt-image-2")(
		context.Background(),
		openaiModel("gpt-image-2"),
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
	response, err := decodeImage(context.Background(), raw)
	if err != nil {
		t.Fatalf("decodeImage: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	_ = capture.body(0)
}

// ---------------------------------------------------------------------------
// Image streaming.
// ---------------------------------------------------------------------------

func openaiImagePartialEvent(b64 string, index int) string {
	return fmt.Sprintf(
		`{"type":"image_generation.partial_image","b64_json":%q,`+
			`"output_format":"png","partial_image_index":%d,`+
			`"created_at":1,"size":"1024x1024","quality":"high","background":"auto"}`,
		b64,
		index,
	)
}

func openaiImageCompletedEvent(
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

func openaiImageEditPartialEvent(b64 string, index int) string {
	return fmt.Sprintf(
		`{"type":"image_edit.partial_image","b64_json":%q,`+
			`"output_format":"png","partial_image_index":%d,`+
			`"created_at":1,"size":"1024x1024","quality":"high","background":"auto"}`,
		b64,
		index,
	)
}

func openaiImageEditCompletedEvent(b64 string) string {
	return fmt.Sprintf(
		`{"type":"image_edit.completed","b64_json":%q,`+
			`"output_format":"png","created_at":2,"size":"1024x1024",`+
			`"quality":"high","background":"auto",`+
			`"usage":{"input_tokens":4,"output_tokens":9,"total_tokens":13}}`,
		b64,
	)
}

func writeOpenAIImageSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
	}
}

func TestImageStreamTransport(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
	)
	if err != nil {
		t.Fatal(err)
	}
	server, capture := newCapturedOpenAI(t, func(
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
		w.Header().Set("x-request-id", "req_openai_stream_1")
		b64 := base64.StdEncoding.EncodeToString(png)
		writeOpenAIImageSSE(
			w,
			openaiImagePartialEvent(b64, 0),
			openaiImagePartialEvent(b64, 1),
			openaiImageCompletedEvent(b64, 7, 13, 20),
			openaiImageCompletedEvent(b64, 7, 13, 20),
		)
	})
	defer server.Close()
	cls := testClients(t, server)

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
	compiled, err := compileImage("gpt-image-2")(
		context.Background(),
		openaiModel("gpt-image-2"),
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
	assertOpenAIImageStreamEvent := func(
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
		if !bytes.Equal(delta.Part.Source.Bytes(), png) {
			t.Errorf("event %d image bytes mismatch", index)
		}
		if event.FinishReason != finish {
			t.Errorf("event %d finish = %q, want %q", index, event.FinishReason, finish)
		}
	}
	assertOpenAIImageStreamEvent(0, 0, true, "")
	assertOpenAIImageStreamEvent(1, 0, true, "")
	assertOpenAIImageStreamEvent(2, 0, false, inference.FinishCompleted)
	assertOpenAIImageStreamEvent(3, 1, false, inference.FinishCompleted)

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
		if events[index].RequestID != "req_openai_stream_1" {
			t.Errorf("completed event %d request_id = %q, want req_openai_stream_1",
				index, events[index].RequestID)
		}
	}
	_ = capture.body(0)
}

func TestImageEditStreamTransport(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
	)
	if err != nil {
		t.Fatal(err)
	}
	source, err := media.NewImageBytes(png, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	server, capture := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		r *http.Request,
		_ map[string]any,
	) {
		if r.URL.Path != "/images/edits" {
			t.Errorf("path = %s, want /images/edits", r.URL.Path)
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
		w.Header().Set("apim-request-id", "req_openai_edit_1")
		b64 := base64.StdEncoding.EncodeToString(png)
		writeOpenAIImageSSE(
			w,
			openaiImageEditPartialEvent(b64, 0),
			openaiImageEditCompletedEvent(b64),
		)
	})
	defer server.Close()
	cls := testClients(t, server)

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
	compiled, err := compileImage("gpt-image-2")(
		context.Background(),
		openaiModel("gpt-image-2"),
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
	if events[1].RequestID != "req_openai_edit_1" {
		t.Errorf("completed request_id = %q, want req_openai_edit_1",
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
		compiled, err := compileImage("gpt-image-2")(
			context.Background(),
			openaiModel("gpt-image-2"),
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
		compiled, err := compileImage("gpt-image-2")(
			context.Background(),
			openaiModel("gpt-image-2"),
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

func TestTTSTransport(t *testing.T) {
	audio := []byte{0x49, 0x44, 0x33, 0x03} // fake mp3 header bytes
	server, capture := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		if voice, _ := body["voice"].(string); voice != "alloy" {
			t.Errorf("voice = %v", body["voice"])
		}
		if format, _ := body["response_format"].(string); format != "mp3" {
			t.Errorf("response_format = %v", body["response_format"])
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	})
	defer server.Close()
	cls := testClients(t, server)

	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "say hi"},
				}},
				Intent: inference.Intent{Audio: &inference.AudioIntent{
					Voice:  media.VoiceSpec{ID: "alloy"},
					Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
				}},
			},
		},
	}
	compiled, err := compileTTS("gpt-4o-mini-tts")(
		context.Background(),
		openaiModel("gpt-4o-mini-tts"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileTTS: %v", err)
	}
	raw, err := transportTTS(cls.api)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("transportTTS: %v", err)
	}
	response, err := decodeTTS(context.Background(), raw)
	if err != nil {
		t.Fatalf("decodeTTS: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	part, ok := response.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if len(part.Source.Bytes()) != len(audio) {
		t.Fatalf("audio = %d bytes", len(part.Source.Bytes()))
	}
	_ = capture.body(0)
}

func TestTTSStreamTransport(t *testing.T) {
	// The speech API streams the audio body directly (no SSE event layer).
	payload := []byte{1, 2, 3, 4, 5, 6}
	server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(payload)
	})
	defer server.Close()
	cls := testClients(t, server)

	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "stream me"},
				}},
				Intent: inference.Intent{Audio: &inference.AudioIntent{
					Voice:  media.VoiceSpec{ID: "alloy"},
					Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
				}},
			},
		},
	}
	operations, err := openTTS(cls, openaiModel("gpt-4o-mini-tts").ID, "default")
	if err != nil {
		t.Fatalf("openTTS: %v", err)
	}
	stream, err := operations.Stream.Stream(
		context.Background(),
		openaiModel("gpt-4o-mini-tts"),
		request,
	)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var audio []byte
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		delta, ok := event.Delta.(inference.AudioPartDelta)
		if !ok {
			continue
		}
		audio = append(audio, delta.Data...)
	}
	if len(audio) != 6 {
		t.Fatalf("streamed audio = %d bytes, want 6", len(audio))
	}
}

// ---------------------------------------------------------------------------
// Reasoning traces — unary and stream.
// ---------------------------------------------------------------------------

func TestGenerateUnaryReasoningItem(t *testing.T) {
	server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		include, _ := body["include"].([]any)
		if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
			t.Errorf("include = %v, want reasoning.encrypted_content", include)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			{
				"type": "reasoning",
				"id":   "rs_1",
				"summary": []map[string]any{
					{"type": "summary_text", "text": "first thought"},
					{"type": "summary_text", "text": "second thought"},
				},
				"encrypted_content": "enc-1",
			},
			textOutputItem("answer"),
		}))
	})
	defer server.Close()
	cls := testClients(t, server)
	operations, err := openGenerate(cls, catalog["gpt-5.6-sol"], openaiModel("gpt-5.6-sol").ID, "default")
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}

	response, err := operations.Unary.Execute(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		simpleTextRequest("hi"),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if response.Metadata.ResponseID != "resp_1" {
		t.Fatalf("response id = %q, want resp_1", response.Metadata.ResponseID)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %+v", parts)
	}
	reasoning, ok := parts[0].(message.ReasoningPart)
	if !ok ||
		reasoning.Text != "first thought\n\nsecond thought" ||
		reasoning.Signature != "enc-1" ||
		reasoning.ID != "rs_1" {
		t.Fatalf("reasoning part = %#v", parts[0])
	}
	if text, ok := parts[1].(message.TextPart); !ok || text.Text != "answer" {
		t.Fatalf("text part = %#v", parts[1])
	}
}

func TestGenerateStreamReasoning(t *testing.T) {
	server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseBody(
			map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"type": "reasoning", "id": "rs_1"},
			},
			map[string]any{
				"type":         "response.reasoning_summary_text.delta",
				"output_index": 0, "item_id": "rs_1", "summary_index": 0,
				"delta": "thinking ",
			},
			map[string]any{
				"type":         "response.reasoning_summary_text.delta",
				"output_index": 0, "item_id": "rs_1", "summary_index": 0,
				"delta": "aloud",
			},
			map[string]any{
				"type": "response.output_item.done", "output_index": 0,
				"item": map[string]any{
					"type": "reasoning", "id": "rs_1",
					"summary": []map[string]any{
						{"type": "summary_text", "text": "thinking aloud"},
					},
					"encrypted_content": "enc-9",
				},
			},
			map[string]any{
				"type": "response.output_item.added", "output_index": 1,
				"item": map[string]any{"type": "message"},
			},
			map[string]any{
				"type": "response.output_text.delta", "output_index": 1, "delta": "done",
			},
			map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": "resp_1", "status": "completed",
					"usage": map[string]any{
						"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
						"input_tokens_details":  map[string]any{"cached_tokens": 0},
						"output_tokens_details": map[string]any{"reasoning_tokens": 1},
					},
				},
			},
		))
	})
	defer server.Close()
	cls := testClients(t, server)
	operations, err := openGenerate(cls, catalog["gpt-5.6-sol"], openaiModel("gpt-5.6-sol").ID, "default")
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}

	stream, err := operations.Stream.Stream(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		simpleTextRequest("hi"),
	)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		_, err = stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	response, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if response.Metadata.ResponseID != "resp_1" {
		t.Fatalf("stream response id = %q, want resp_1", response.Metadata.ResponseID)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %+v", parts)
	}
	reasoning, ok := parts[0].(message.ReasoningPart)
	if !ok ||
		reasoning.Text != "thinking aloud" ||
		reasoning.Signature != "enc-9" ||
		reasoning.ID != "rs_1" {
		t.Fatalf("streamed reasoning = %#v", parts[0])
	}
	if text, ok := parts[1].(message.TextPart); !ok || text.Text != "done" {
		t.Fatalf("text part = %#v", parts[1])
	}
}
