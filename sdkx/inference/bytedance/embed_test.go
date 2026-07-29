package bytedance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/media"
)

func embedModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "bytedance", Name: name},
		Profile: "default",
	}
}

func TestEmbedTextCapturedWire(t *testing.T) {
	server, capture := newCapturedArk(t, func(w http.ResponseWriter, body map[string]any, _ bool) {
		inputs, _ := body["input"].([]any)
		data := make([]map[string]any, 0, len(inputs))
		for index := range inputs {
			data = append(data, map[string]any{
				"object":    "embedding",
				"index":     index,
				"embedding": []float64{0.1 * float64(index+1), 0.2},
			})
		}
		payload, _ := json.Marshal(map[string]any{
			"id":     "embd_1",
			"object": "list",
			"data":   data,
			"usage":  map[string]any{"prompt_tokens": 9, "total_tokens": 9},
		})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, string(payload))
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	dimensions := 2
	response, err := runtime.Embed(
		context.Background(),
		embedModel("doubao-embedding-large"),
		inference.EmbedRequest{
			Items: []inference.EmbedItem{
				{Content: inference.Content{Parts: []inference.Part{inference.TextPart{Text: "first"}}}},
				{Content: inference.Content{Parts: []inference.Part{inference.TextPart{Text: "second"}}}},
			},
			Dimensions: &dimensions,
		},
	)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(response.Embeddings) != 2 {
		t.Fatalf("embeddings = %d", len(response.Embeddings))
	}
	if response.Embeddings[1].Vector[0] != float32(0.2) {
		t.Fatalf("order not preserved: %+v", response.Embeddings)
	}
	if response.Usage.InputTokens != 9 || response.Usage.ItemCount != 2 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	body := capture.body(0)
	if body["model"] != "doubao-embedding-large" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["dimensions"].(float64) != 2 {
		t.Fatalf("dimensions = %v", body["dimensions"])
	}
	input, _ := body["input"].([]any)
	if len(input) != 2 || input[0] != "first" || input[1] != "second" {
		t.Fatalf("input = %v", body["input"])
	}
}

func TestEmbedMultimodalPerItemCalls(t *testing.T) {
	server, capture := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		payload, _ := json.Marshal(map[string]any{
			"id": "embd_mm",
			"data": map[string]any{
				"object":    "embedding",
				"embedding": []float64{0.5, 0.6},
			},
			"usage": map[string]any{"prompt_tokens": 4, "total_tokens": 4},
		})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, string(payload))
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	image, err := media.NewImageURL("https://example.com/i.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtime.Embed(
		context.Background(),
		embedModel("doubao-embedding-vision"),
		inference.EmbedRequest{
			Items: []inference.EmbedItem{
				{Content: inference.Content{Parts: []inference.Part{
					inference.TextPart{Text: "caption"},
					inference.ImagePart{Source: image},
				}}},
				{Content: inference.Content{Parts: []inference.Part{
					inference.TextPart{Text: "text only"},
				}}},
			},
		},
	)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(response.Embeddings) != 2 {
		t.Fatalf("embeddings = %d", len(response.Embeddings))
	}
	if response.Usage.InputTokens != 8 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	// The multimodal endpoint fuses one item per call: two items, two calls.
	if len(capture.bodies) != 2 {
		t.Fatalf("calls = %d", len(capture.bodies))
	}
	input, _ := capture.body(0)["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("first call inputs = %v", capture.body(0)["input"])
	}
	imageInput, _ := input[1].(map[string]any)
	if imageInput["type"] != "image_url" {
		t.Fatalf("image input = %v", input[1])
	}
	imageURL, _ := imageInput["image_url"].(map[string]any)
	if imageURL["url"] != "https://example.com/i.png" {
		t.Fatalf("image url = %v", imageInput)
	}
}

func TestEmbedRejections(t *testing.T) {
	image, err := media.NewImageURL("https://example.com/i.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		model   string
		request inference.EmbedRequest
		field   inference.FieldID
	}{
		{
			name:  "image on text model",
			model: "doubao-embedding-large",
			request: inference.EmbedRequest{Items: []inference.EmbedItem{
				{Content: inference.Content{Parts: []inference.Part{inference.ImagePart{Source: image}}}},
			}},
			field: inference.FieldEmbedItemImage,
		},
		{
			name:  "multi-part on text model",
			model: "doubao-embedding-large",
			request: inference.EmbedRequest{Items: []inference.EmbedItem{
				{Content: inference.Content{Parts: []inference.Part{
					inference.TextPart{Text: "a"},
					inference.TextPart{Text: "b"},
				}}},
			}},
			field: inference.FieldEmbedItemMultiPart,
		},
		{
			name:  "audio part",
			model: "doubao-embedding-vision",
			request: inference.EmbedRequest{Items: []inference.EmbedItem{
				{Content: inference.Content{Parts: []inference.Part{
					inference.AudioPart{Source: mustAudio(t)},
				}}},
			}},
			field: inference.FieldEmbedItemAudio,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, capture := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
				t.Error("transport must not run after compiler rejection")
			})
			defer server.Close()
			runtime := newTestRuntime(t, server)
			_, err := runtime.Embed(context.Background(), embedModel(tc.model), tc.request)
			if err == nil {
				t.Fatal("expected compiler rejection")
			}
			if !inference.IsKind(err, inference.UnsupportedFeature) {
				t.Fatalf("kind = %v", err)
			}
			var inferenceErr *inference.Error
			if !errors.As(err, &inferenceErr) || inferenceErr.Field != tc.field {
				t.Fatalf("field = %v, want %s", err, tc.field)
			}
			if len(capture.bodies) != 0 {
				t.Fatalf("transport ran %d times", len(capture.bodies))
			}
		})
	}
}

func mustAudio(t *testing.T) media.AudioSource {
	t.Helper()
	source, err := media.NewAudioBytes([]byte{1, 2, 3}, "audio/wav")
	if err != nil {
		t.Fatal(err)
	}
	return source
}
