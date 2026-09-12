package bytedance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
)

func embedDimensionRequest() inference.EmbedRequest {
	dimensions := 512
	return inference.EmbedRequest{
		Items: []inference.EmbedItem{{
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "hi"},
			}},
		}},
		Dimensions: &dimensions,
	}
}

// TestEmbedDimensionsGate locks the capability-driven gate: models without
// the published capability reject the dimensions field, and models that
// publish it compile it through.
func TestEmbedDimensionsGate(t *testing.T) {
	fixed := catalogEntry{
		kind:         kindEmbed,
		capabilities: model.ModelCapabilities{}.WithInputs(message.PartText),
	}
	compiled, err := compileEmbed("fixed", fixed)(
		context.Background(),
		conformanceModel("fixed"),
		embedDimensionRequest(),
	)
	if err == nil {
		t.Fatal("fixed-size model unexpectedly accepted custom dimensions")
	}
	if !compiled.Report.Rejects(inference.FieldEmbedDimensions) {
		t.Fatal("dimensions request was not rejected on the dimensions field")
	}

	compiled, err = compileEmbed(
		"doubao-embedding-large",
		catalog["doubao-embedding-large"],
	)(
		context.Background(),
		conformanceModel("doubao-embedding-large"),
		embedDimensionRequest(),
	)
	if err != nil {
		t.Fatalf("dimensions request rejected on a capable model: %v", err)
	}
	if compiled.Wire.text == nil || compiled.Wire.text.Dimensions != 512 {
		t.Fatalf("embed request = %+v, want dimensions 512", compiled.Wire.text)
	}
}

// TestCustomEmbedDimensionsRequiresEmbedKind guards the capability leaf's
// family contract on the spec side.
func TestCustomEmbedDimensionsRequiresEmbedKind(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "m",
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "custom_embed_dimensions": true}
		}]
	}`)); err == nil {
		t.Fatal("custom_embed_dimensions on a generate model unexpectedly accepted")
	}
}

// TestCustomEmbedDimensionsFalseOnGenerateAccepted guards the leaf
// contract: an explicit false is the conservative declaration and must be
// a harmless no-op on models that never embed.
func TestCustomEmbedDimensionsFalseOnGenerateAccepted(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "m",
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "custom_embed_dimensions": false}
		}]
	}`)); err != nil {
		t.Fatalf("explicit false on a generate model rejected: %v", err)
	}
}

// TestEmbedCompileToTransportText closes the loop the wire used to close: one
// canonical request compiles into the batched text body the endpoint receives.
func TestEmbedCompileToTransportText(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/embeddings" {
			t.Errorf("path = %q, want /embeddings", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"id": "emb-1",
			"model": "doubao-embedding-large",
			"data": [
				{"object": "embedding", "index": 0, "embedding": [1, 2]},
				{"object": "embedding", "index": 1, "embedding": [3, 4]}
			],
			"usage": {"prompt_tokens": 9, "total_tokens": 9}
		}`))
	}))
	defer server.Close()

	dimensions := 512
	request := inference.EmbedRequest{
		Items: []inference.EmbedItem{
			{Content: message.NewTextContent("hi")},
			{Content: message.NewTextContent("there")},
		},
		Dimensions: &dimensions,
	}
	compiled, err := compileEmbed(
		"doubao-embedding-large",
		catalog["doubao-embedding-large"],
	)(context.Background(), conformanceModel("doubao-embedding-large"), request)
	if err != nil {
		t.Fatalf("compileEmbed: %v", err)
	}

	raw, err := transportEmbed(arkTestClient(t, server), nil)(
		context.Background(),
		compiled.Wire,
	)
	if err != nil {
		t.Fatalf("transportEmbed: %v", err)
	}
	if len(raw.vectors) != 2 || len(raw.vectors[1]) != 2 {
		t.Fatalf("vectors = %#v, want two vectors", raw.vectors)
	}
	for key, want := range map[string]any{
		"model":           "doubao-embedding-large",
		"dimensions":      float64(512),
		"encoding_format": "float",
	} {
		if body[key] != want {
			t.Errorf("body[%q] = %#v, want %#v", key, body[key], want)
		}
	}
	inputs, ok := body["input"].([]any)
	if !ok || len(inputs) != 2 || inputs[0] != "hi" || inputs[1] != "there" {
		t.Errorf("input = %#v, want [hi there]", body["input"])
	}
}

// TestEmbedCompileToTransportMultimodal closes the same loop for the fusion
// endpoint: one request per item, with the item's text and image inputs in the
// order the canonical content listed them.
func TestEmbedCompileToTransportMultimodal(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/embeddings/multimodal" {
			t.Errorf("path = %q, want /embeddings/multimodal", request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		bodies = append(bodies, body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"id": "emb-multi",
			"model": "doubao-embedding-vision",
			"data": {"object": "embedding", "embedding": [1, 2, 3]},
			"usage": {"prompt_tokens": 4, "total_tokens": 4}
		}`))
	}))
	defer server.Close()

	source, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	request := inference.EmbedRequest{
		Items: []inference.EmbedItem{{
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "a cat"},
				message.ImagePart{Source: source},
				message.TextPart{Text: "?"},
			}},
		}},
	}
	compiled, err := compileEmbed(
		"doubao-embedding-vision",
		catalog["doubao-embedding-vision"],
	)(context.Background(), conformanceModel("doubao-embedding-vision"), request)
	if err != nil {
		t.Fatalf("compileEmbed: %v", err)
	}
	if compiled.Wire.text != nil {
		t.Fatal("multimodal model compiled the batched text shape")
	}

	raw, err := transportEmbed(arkTestClient(t, server), nil)(
		context.Background(),
		compiled.Wire,
	)
	if err != nil {
		t.Fatalf("transportEmbed: %v", err)
	}
	if len(raw.vectors) != 1 {
		t.Fatalf("vectors = %#v, want one fused vector", raw.vectors)
	}
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want one per item", len(bodies))
	}
	inputs, ok := bodies[0]["input"].([]any)
	if !ok || len(inputs) != 3 {
		t.Fatalf("input = %#v, want text, image, text", bodies[0]["input"])
	}
	first := inputs[0].(map[string]any)
	second := inputs[1].(map[string]any)
	third := inputs[2].(map[string]any)
	if first["type"] != "text" || first["text"] != "a cat" {
		t.Errorf("input[0] = %#v, want the leading text", first)
	}
	if second["type"] != "image_url" {
		t.Errorf("input[1] = %#v, want the image", second)
	}
	imageURL, _ := second["image_url"].(map[string]any)
	if imageURL["url"] != "https://example.com/cat.png" {
		t.Errorf("input[1].image_url = %#v, want the reference url", imageURL)
	}
	if third["type"] != "text" || third["text"] != "?" {
		t.Errorf("input[2] = %#v, want the trailing text", third)
	}
}

// arkTestClient points an Ark SDK client at the captured test server.
func arkTestClient(t *testing.T, server *httptest.Server) *arkruntime.Client {
	t.Helper()
	return arkruntime.NewClientWithApiKey(
		"sk-test",
		arkruntime.WithBaseUrl(server.URL),
		arkruntime.WithHTTPClient(server.Client()),
		arkruntime.WithRetryTimes(0),
	)
}
