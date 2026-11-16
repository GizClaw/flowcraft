package bytedance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/driver/internal/httpx"
)

// embedCall is one request as the server saw it: the payload it carried and
// where it went. Keeping them apart means the payload map is exactly what was
// sent.
type embedCall struct {
	path    string
	query   string
	payload map[string]any
}

// embedServer answers the two embedding surfaces. It records what the request
// carried, because the wire shape is the part a vendor is strict about: the
// text batch is `input: ["a","b"]`, the fused one is a list of items.
func embedServer(t *testing.T, seen *[]embedCall) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		*seen = append(*seen, embedCall{path: r.URL.Path, query: r.URL.RawQuery, payload: payload})
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case embeddingPath:
			// Index order is not guaranteed on the wire: the transport has to
			// place each vector by its index, not by arrival.
			_, _ = w.Write([]byte(`{"usage":{"total_tokens":7},"data":[
				{"index":1,"embedding":[0.3,0.4]},
				{"index":0,"embedding":[0.1,0.2]}]}`))
		case multimodalEmbeddingPath:
			// Each fused item answers with its own vector, so the order they are
			// assembled in is observable.
			_, _ = w.Write([]byte(`{"usage":{"total_tokens":3},"data":{"embedding":[` +
				strconv.FormatFloat(float64(len(*seen)), 'f', 1, 64) + `,0.6]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// The text batch is one call for the whole batch, and the answer's indexes are
// what put the vectors in request order: a platform may answer out of order, and
// a vector attached to the wrong text is a silent retrieval bug.
func TestTransportEmbedText(t *testing.T) {
	var seen []embedCall
	server := embedServer(t, &seen)
	client := httpx.New(httpx.Config{
		BaseURL: server.URL, APIKey: "key",
		Headers: map[string]string{"X-Project-Name": "proj"},
	})
	query := url.Values{"region": []string{"cn-beijing"}}

	raw, err := transportEmbedText(context.Background(), client, query, embeddingRequest{
		Model: "ep-embed", Input: []string{"first", "second"}, EncodingFormat: "float",
	})
	if err != nil {
		t.Fatalf("text embed: %v", err)
	}
	if len(raw.vectors) != 2 {
		t.Fatalf("vectors = %d, want 2", len(raw.vectors))
	}
	if raw.vectors[0][0] != 0.1 || raw.vectors[1][0] != 0.3 {
		t.Errorf("vectors = %v, want them ordered by index", raw.vectors)
	}
	if raw.inputTokens != 7 {
		t.Errorf("tokens = %d, want 7", raw.inputTokens)
	}
	if len(seen) != 1 {
		t.Fatalf("calls = %d, want one for the whole batch", len(seen))
	}
	if seen[0].path != embeddingPath {
		t.Errorf("path = %v, want %s", seen[0].path, embeddingPath)
	}
	if seen[0].query != "region=cn-beijing" {
		t.Errorf("query = %v, want the deployment's parameters", seen[0].query)
	}
	// The batch has to reach the platform in the order the caller wrote it, and
	// each string has to be the one asked for: a length check would accept a
	// reversed or duplicated batch.
	input, ok := seen[0].payload["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("input = %v, want two strings", seen[0].payload["input"])
	}
	if input[0] != "first" || input[1] != "second" {
		t.Errorf("input = %v, want [first second] in order", input)
	}
}

// The fused surface is one call per item, because each answer is one vector for
// one text-and-image pair; the batch is assembled from the answers in order.
func TestTransportEmbedMultimodal(t *testing.T) {
	var seen []embedCall
	server := embedServer(t, &seen)
	client := httpx.New(httpx.Config{BaseURL: server.URL, APIKey: "key"})
	requests := []multimodalEmbeddingRequest{
		{Model: "ep-vision", Input: []multimodalEmbeddingInput{{Type: "text", Text: "a"}}},
		{Model: "ep-vision", Input: []multimodalEmbeddingInput{
			{Type: "text", Text: "b"},
			{Type: "image_url", ImageURL: &multimodalImageURL{URL: "https://example.test/x.png"}},
		}},
	}
	raw, err := transportEmbedMultimodal(context.Background(), client, nil, requests)
	if err != nil {
		t.Fatalf("multimodal embed: %v", err)
	}
	// The server answers each call with the vector of its own sequence number, so
	// the assembled batch also pins the order the calls went out in.
	if len(raw.vectors) != 2 || raw.vectors[0][0] != 1 || raw.vectors[1][0] != 2 {
		t.Errorf("vectors = %v, want each answer at its own position", raw.vectors)
	}
	if raw.inputTokens != 6 {
		t.Errorf("tokens = %d, want 6 from two calls", raw.inputTokens)
	}
	if len(seen) != 2 {
		t.Fatalf("calls = %d, want one per item", len(seen))
	}
	if seen[1].path != multimodalEmbeddingPath {
		t.Errorf("path = %v", seen[1].path)
	}
	if model, _ := seen[1].payload["model"].(string); model != "ep-vision" {
		t.Errorf("model = %v", seen[1].payload["model"])
	}
}

// An index outside the batch is the platform contradicting its own request, and
// the turn fails rather than dropping a vector: a missing embedding would show
// up much later as a ranking that cannot be explained.
func TestTransportEmbedTextRefusesABadIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1},"data":[{"index":5,"embedding":[0.1]}]}`))
	}))
	t.Cleanup(server.Close)
	_, err := transportEmbedText(context.Background(),
		httpx.New(httpx.Config{BaseURL: server.URL}), nil, embeddingRequest{
			Model: "ep-embed", Input: []string{"only"},
		})
	if err == nil {
		t.Fatal("an out-of-range index was accepted")
	}
	if !strings.Contains(err.Error(), "index 5 out of range") {
		t.Fatalf("error = %v", err)
	}
}

// embedRequest routes to the surface its shape names, which is the only
// difference between the two transports.
func TestTransportEmbedRoutesByShape(t *testing.T) {
	var seen []embedCall
	server := embedServer(t, &seen)
	client := httpx.New(httpx.Config{BaseURL: server.URL})
	transport := transportEmbed(client, nil)
	ctx := context.Background()

	text := &embedRequest{text: &embeddingRequest{Model: "ep-embed", Input: []string{"a"}}}
	if _, err := transport(ctx, inference.EmbedRequest{}, text); err != nil {
		t.Fatalf("text route: %v", err)
	}
	fused := &embedRequest{multimodal: []multimodalEmbeddingRequest{
		{Model: "ep-vision", Input: []multimodalEmbeddingInput{{Type: "text", Text: "b"}}},
	}}
	if _, err := transport(ctx, inference.EmbedRequest{}, fused); err != nil {
		t.Fatalf("fused route: %v", err)
	}
	if len(seen) != 2 || seen[0].path != embeddingPath ||
		seen[1].path != multimodalEmbeddingPath {
		t.Fatalf("paths = %v", seen)
	}
}

// The decoder is the last gate before the vectors reach a caller: an empty
// vector is a provider failure, not an embedding of zero similarity.
func TestDecodeEmbedRefusesAnEmptyVector(t *testing.T) {
	request := inference.EmbedRequest{
		Model: modelRef("ep-embed"),
		Input: []message.Content{message.NewTextContent("a")},
	}
	if _, err := decodeEmbed(context.Background(), request,
		embedRaw{vectors: [][]float32{{0.1}, {}}}); err == nil {
		t.Fatal("an empty vector was accepted")
	}
	response, err := decodeEmbed(context.Background(), request,
		embedRaw{vectors: [][]float32{{0.1, 0.2}}, inputTokens: 4})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Vectors) != 1 || response.Usage.InputTokens != 4 {
		t.Errorf("response = %+v", response)
	}
	if response.Metadata.Model != request.Model.ID {
		t.Errorf("model = %v", response.Metadata.Model)
	}
}

// modelRef keeps these tables readable.
func modelRef(name string) model.ModelRef {
	return model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}}
}
