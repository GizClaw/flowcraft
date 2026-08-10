package qwen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

func intPointer(value int) *int       { return &value }
func int64Pointer(value int64) *int64 { return &value }

// floatArray renders a JSON float array of n dimensions: canonical
// response validation rejects vectors that miss the requested size.
func floatArray(n int, value float64) string {
	parts := make([]string, n)
	for index := range parts {
		parts[index] = fmt.Sprintf("%v", value)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func textEmbedRequest() inference.EmbedRequest {
	return inference.EmbedRequest{
		Items: []inference.EmbedItem{{
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "风急天高猿啸哀"},
			}},
		}},
	}
}

func TestTextEmbeddingOnWire(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, body map[string]any) {
		dimension := 1024
		if requested, ok := body["parameters"].(map[string]any)["dimension"].(float64); ok {
			dimension = int(requested)
		}
		_, _ = fmt.Fprintf(w, `{
			"request_id": "emb-1",
			"output": {"embeddings": [
				{"embedding": %s, "text_index": 1},
				{"embedding": %s, "text_index": 0}
			]},
			"usage": {"total_tokens": 27}
		}`, floatArray(dimension, 0.5), floatArray(dimension, 0.1))
	})

	runtime := newTestRuntime(t, server)
	response, err := runtime.Embed(
		context.Background(),
		qwenModel("text-embedding-v4"),
		inference.EmbedRequest{
			Items: []inference.EmbedItem{
				{Content: message.Content{Parts: []message.Part{message.TextPart{Text: "a"}}}},
				{Content: message.Content{Parts: []message.Part{message.TextPart{Text: "b"}}}},
			},
			Dimensions: intPointer(512),
			Extensions: inference.Extensions{EmbedOptions{
				TextType: "query",
				Instruct: "Retrieve similar reviews",
			}},
		},
	)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if path := server.path(t, 0); path != pathTextEmbedding {
		t.Fatalf("path = %q", path)
	}
	body := server.body(t, 0)
	if body["model"] != "text-embedding-v4" {
		t.Fatalf("model = %v", body["model"])
	}
	texts, ok := body["input"].(map[string]any)["texts"].([]any)
	if !ok || len(texts) != 2 || texts[0] != "a" || texts[1] != "b" {
		t.Fatalf("texts = %v", body["input"])
	}
	parameters := body["parameters"].(map[string]any)
	if parameters["dimension"] != float64(512) ||
		parameters["text_type"] != "query" ||
		parameters["instruct"] != "Retrieve similar reviews" {
		t.Fatalf("parameters = %v", parameters)
	}

	// Vectors realign to canonical item order despite text_index ordering.
	if len(response.Embeddings) != 2 {
		t.Fatalf("embeddings = %+v", response.Embeddings)
	}
	if response.Embeddings[0].Vector[0] != 0.1 || response.Embeddings[1].Vector[0] != 0.5 {
		t.Fatalf("vectors not realigned: %+v", response.Embeddings)
	}
	if response.Usage.InputTokens != 27 || response.Usage.ItemCount != 2 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if response.Metadata.RequestID != "emb-1" {
		t.Fatalf("request id = %q, want emb-1", response.Metadata.RequestID)
	}
}

func TestMultimodalEmbeddingIndependent(t *testing.T) {
	image, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	video, err := media.NewVideoURL("https://example.com/clip.mp4", "video/mp4")
	if err != nil {
		t.Fatal(err)
	}

	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, `{
			"output": {"embeddings": [
				{"index": 0, "embedding": [1], "type": "text"},
				{"index": 1, "embedding": [2], "type": "image"},
				{"index": 2, "embedding": [3], "type": "video"}
			]},
			"usage": {"input_tokens": 43, "image_tokens": 1247, "total_tokens": 1290}
		}`)
	})

	runtime := newTestRuntime(t, server)
	response, err := runtime.Embed(
		context.Background(),
		qwenModel("qwen3-vl-embedding"),
		inference.EmbedRequest{
			Items: []inference.EmbedItem{
				{Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}}},
				{Content: message.Content{Parts: []message.Part{message.ImagePart{Source: image}}}},
				{Content: message.Content{Parts: []message.Part{message.VideoPart{Source: video}}}},
			},
		},
	)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if path := server.path(t, 0); path != pathMultimodalEmbedding {
		t.Fatalf("path = %q", path)
	}
	body := server.body(t, 0)
	contents, ok := body["input"].(map[string]any)["contents"].([]any)
	if !ok || len(contents) != 3 {
		t.Fatalf("contents = %v", body["input"])
	}
	if contents[0].(map[string]any)["text"] != "hi" ||
		contents[1].(map[string]any)["image"] != "https://example.com/cat.png" ||
		contents[2].(map[string]any)["video"] != "https://example.com/clip.mp4" {
		t.Fatalf("contents = %v", contents)
	}
	if fusion, exists := body["parameters"].(map[string]any)["enable_fusion"]; exists && fusion != false {
		t.Fatal("independent batch must not enable fusion")
	}
	if len(response.Embeddings) != 3 || response.Usage.InputTokens != 1290 {
		t.Fatalf("response = %+v", response)
	}
}

func TestMultimodalEmbeddingFusionPerItem(t *testing.T) {
	image, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}

	server := newDashServer(t, func(w http.ResponseWriter, body map[string]any) {
		dimension := 2560
		if requested, ok := body["parameters"].(map[string]any)["dimension"].(float64); ok {
			dimension = int(requested)
		}
		_, _ = fmt.Fprintf(w, `{
			"output": {"embeddings": [{"index": 0, "embedding": %s, "type": "fusion"}]},
			"usage": {"input_tokens": 10, "image_tokens": 90, "total_tokens": 100}
		}`, floatArray(dimension, 0.7))
	})

	runtime := newTestRuntime(t, server)
	response, err := runtime.Embed(
		context.Background(),
		qwenModel("qwen3-vl-embedding"),
		inference.EmbedRequest{
			Items: []inference.EmbedItem{
				{Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "a sneaker"},
					message.ImagePart{Source: image},
				}}},
				{Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "a boot"},
					message.ImagePart{Source: image},
				}}},
			},
			Dimensions: intPointer(1024),
		},
	)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if requests := server.requests(); requests != 2 {
		t.Fatalf("fusion must issue one request per item, got %d", requests)
	}
	for index := 0; index < 2; index++ {
		body := server.body(t, index)
		parameters := body["parameters"].(map[string]any)
		if parameters["enable_fusion"] != true {
			t.Fatalf("fusion body missing enable_fusion: %v", body)
		}
		if parameters["dimension"] != float64(1024) {
			t.Fatalf("dimension = %v", parameters["dimension"])
		}
		contents := body["input"].(map[string]any)["contents"].([]any)
		if len(contents) != 2 {
			t.Fatalf("fusion contents = %v", contents)
		}
	}
	if len(response.Embeddings) != 2 || response.Usage.InputTokens != 200 {
		t.Fatalf("response = %+v", response)
	}
}

func TestEmbedCompileRejections(t *testing.T) {
	image, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	videoData, err := media.NewVideoBytes(make([]byte, 64), "video/mp4")
	if err != nil {
		t.Fatal(err)
	}

	suites := []struct {
		name       string
		model      string
		rejections []inferencetest.CompilerRejection[inference.EmbedRequest]
	}{
		{
			name:  "text-embedding-v4",
			model: "text-embedding-v4",
			rejections: []inferencetest.CompilerRejection[inference.EmbedRequest]{
				{
					Name: "unsupported dimensions",
					Request: func() inference.EmbedRequest {
						request := textEmbedRequest()
						request.Dimensions = intPointer(42)
						return request
					},
					Field: inference.FieldEmbedDimensions,
					Kind:  inference.UnsupportedFeature,
				},
				{
					Name: "image part",
					Request: func() inference.EmbedRequest {
						return inference.EmbedRequest{
							Items: []inference.EmbedItem{{
								Content: message.Content{Parts: []message.Part{
									message.ImagePart{Source: image},
								}},
							}},
						}
					},
					Field: inference.FieldEmbedItemImage,
					Kind:  inference.UnsupportedFeature,
				},
				{
					Name: "multi-part item",
					Request: func() inference.EmbedRequest {
						return inference.EmbedRequest{
							Items: []inference.EmbedItem{{
								Content: message.Content{Parts: []message.Part{
									message.TextPart{Text: "a"},
									message.TextPart{Text: "b"},
								}},
							}},
						}
					},
					Field: inference.FieldEmbedItemMultiPart,
					Kind:  inference.UnsupportedFeature,
				},
				{
					Name: "too many rows",
					Request: func() inference.EmbedRequest {
						items := make([]inference.EmbedItem, maxTextEmbedRows+1)
						for index := range items {
							items[index] = inference.EmbedItem{Content: message.Content{
								Parts: []message.Part{message.TextPart{Text: "x"}},
							}}
						}
						return inference.EmbedRequest{Items: items}
					},
					Field: inference.FieldEmbedItems,
					Kind:  inference.UnsupportedFeature,
				},
			},
		},
		{
			name:  "qwen3-vl-embedding",
			model: "qwen3-vl-embedding",
			rejections: []inferencetest.CompilerRejection[inference.EmbedRequest]{
				{
					Name: "unsupported dimensions",
					Request: func() inference.EmbedRequest {
						request := textEmbedRequest()
						request.Dimensions = intPointer(42)
						return request
					},
					Field: inference.FieldEmbedDimensions,
					Kind:  inference.UnsupportedFeature,
				},
				{
					Name: "inline video",
					Request: func() inference.EmbedRequest {
						return inference.EmbedRequest{
							Items: []inference.EmbedItem{{
								Content: message.Content{Parts: []message.Part{
									message.VideoPart{Source: videoData},
								}},
							}},
						}
					},
					Field: inference.FieldEmbedItemVideo,
					Kind:  inference.UnsupportedFeature,
				},
				{
					Name: "text_type on multimodal model",
					Request: func() inference.EmbedRequest {
						request := textEmbedRequest()
						request.Extensions = inference.Extensions{EmbedOptions{TextType: "query"}}
						return request
					},
					Field: inference.ExtensionField("text_type").Qualify(EmbedOptions{}),
					Kind:  inference.InvalidExtension,
				},
			},
		},
	}

	for _, suite := range suites {
		t.Run(suite.name, func(t *testing.T) {
			inferencetest.RunCompiler(t, inferencetest.CompilerSuite[inference.EmbedRequest, embedWire]{
				Operation: inference.OperationEmbed,
				Model:     qwenModel(suite.model),
				Request:   textEmbedRequest,
				Snapshot: func(request inference.EmbedRequest) any {
					return request.Clone()
				},
				Fields: func(request inference.EmbedRequest) []inference.FieldID {
					return request.ActiveFields()
				},
				Compile:    compileEmbed(suite.model, catalog[suite.model]),
				Rejections: suite.rejections,
			})
		})
	}
}

func TestEmbedRejectsForeignExtension(t *testing.T) {
	request := textEmbedRequest()
	request.Extensions = inference.Extensions{GenerateOptions{TopK: int64Pointer(10)}}
	_, err := compileEmbed("text-embedding-v4", catalog["text-embedding-v4"])(
		context.Background(),
		qwenModel("text-embedding-v4"),
		request,
	)
	if !inference.IsKind(err, inference.InvalidExtension) {
		t.Fatalf("error = %v, want invalid_extension", err)
	}
}

func TestEmbedDataPartLowersToText(t *testing.T) {
	request := inference.EmbedRequest{Items: []inference.EmbedItem{{
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "caption"},
			message.DataPart{
				MediaType: "application/vnd.example",
				Value:     json.RawMessage(`{"k":1}`),
			},
		}},
	}}}
	compiled, err := compileEmbed("text-embedding-v4", catalog["text-embedding-v4"])(
		context.Background(), qwenModel("text-embedding-v4"), request,
	)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(compiled.Wire.Texts) != 1 ||
		!strings.Contains(compiled.Wire.Texts[0], `{"k":1}`) {
		t.Fatalf("wire texts = %+v", compiled.Wire.Texts)
	}
}

func TestMultimodalEmbedDataPartLowersToText(t *testing.T) {
	image, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	request := inference.EmbedRequest{Items: []inference.EmbedItem{{
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "caption"},
			message.DataPart{
				MediaType: "application/vnd.example",
				Value:     json.RawMessage(`{"k":1}`),
			},
			message.ImagePart{Source: image},
		}},
	}}}
	compiled, err := compileEmbed("qwen3-vl-embedding", catalog["qwen3-vl-embedding"])(
		context.Background(), qwenModel("qwen3-vl-embedding"), request,
	)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if compiled.Wire.Shape != embedShapeFusion || len(compiled.Wire.Items) != 1 {
		t.Fatalf("wire = %+v", compiled.Wire)
	}
	found := false
	for _, content := range compiled.Wire.Items[0] {
		if content.Text != nil && strings.Contains(*content.Text, `{"k":1}`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("wire contents = %+v", compiled.Wire.Items[0])
	}
}

func TestEmbedEnvelopeError(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, `{"code": "InvalidApiKey", "message": "bad key", "request_id": "x"}`)
	})

	runtime := newTestRuntime(t, server)
	_, err := runtime.Embed(
		context.Background(),
		qwenModel("text-embedding-v4"),
		textEmbedRequest(),
	)
	if !errdefs.IsUnauthorized(err) {
		t.Fatalf("error = %v, want unauthorized", err)
	}
	var inferenceErr *inference.Error
	if !errors.As(err, &inferenceErr) || inferenceErr.RequestID != "x" {
		t.Fatalf("error request id = %+v, want x", inferenceErr)
	}
}

func TestEmbedFusionServerReturnsMultiple(t *testing.T) {
	image, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, `{
			"output": {"embeddings": [
				{"index": 0, "embedding": [1], "type": "text"},
				{"index": 1, "embedding": [2], "type": "image"}
			]},
			"usage": {"total_tokens": 10}
		}`)
	})

	runtime := newTestRuntime(t, server)
	_, err = runtime.Embed(
		context.Background(),
		qwenModel("qwen3-vl-embedding"),
		inference.EmbedRequest{
			Items: []inference.EmbedItem{{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "a"},
					message.ImagePart{Source: image},
				}},
			}},
		},
	)
	if !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("error = %v, want provider_failure", err)
	}
}
