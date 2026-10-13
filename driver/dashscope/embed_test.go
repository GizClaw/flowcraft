package dashscope

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

func embedTextItem(text string) inference.EmbedItem {
	return inference.EmbedItem{
		Content: message.Content{Parts: []message.Part{message.TextPart{Text: text}}},
	}
}

func TestCompileEmbed(t *testing.T) {
	declared := testDeclaration(t, "qwen3.7-text-embedding")
	request := inference.EmbedRequest{
		Items: []inference.EmbedItem{
			embedTextItem("alpha"),
			embedTextItem("beta"),
			embedTextItem("gamma"),
		},
		Dimensions: intPtr(1024),
		Extensions: inference.Extensions{
			EmbedOptions{
				Provider: providerID,
				TextType: "query",
				Instruct: "retrieve passages that answer the query",
			},
		},
	}
	wire, report, err := compileEmbedWire(t, declared, request)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.model != "qwen3.7-text-embedding" {
		t.Errorf("model = %q", wire.model)
	}
	if len(wire.batches) != 2 ||
		len(wire.batches[0]) != 2 || len(wire.batches[1]) != 1 {
		t.Fatalf("batches = %#v", wire.batches)
	}
	if wire.batches[0][0] != "alpha" || wire.batches[1][0] != "gamma" {
		t.Errorf("batches = %#v", wire.batches)
	}
	if wire.dimension == nil || *wire.dimension != 1024 {
		t.Errorf("dimension = %v", wire.dimension)
	}
	if wire.textType != "query" {
		t.Errorf("text type = %q", wire.textType)
	}
	if wire.instruct == "" {
		t.Error("the retrieval instruction was dropped")
	}
	if reason := rejectedReason(report, inference.FieldEmbedDimensions); reason != "" {
		t.Errorf("dimensions rejected: %s", reason)
	}
}

func TestCompileEmbedRejections(t *testing.T) {
	declared := testDeclaration(t, "qwen3.7-text-embedding")

	// A size outside the declared whitelist cannot be honored by the
	// platform's exact-size constraint.
	request := inference.EmbedRequest{
		Items:      []inference.EmbedItem{embedTextItem("alpha")},
		Dimensions: intPtr(768),
	}
	_, report, err := compileEmbedWire(t, declared, request)
	if err == nil {
		t.Fatal("an undeclared dimension compiled")
	}
	if reason := rejectedReason(report, inference.FieldEmbedDimensions); !strings.Contains(reason, "accepts dimensions 2560, 1024") {
		t.Errorf("dimensions reason = %q", reason)
	}

	// The instruction is a query-side control.
	request = inference.EmbedRequest{
		Items: []inference.EmbedItem{embedTextItem("alpha")},
		Extensions: inference.Extensions{
			EmbedOptions{Provider: providerID, Instruct: "find passages"},
		},
	}
	_, report, err = compileEmbedWire(t, declared, request)
	if err == nil {
		t.Fatal("an instruction compiled without the query role")
	}
	if reason := rejectedReason(report, embedOptionsField("instruct")); !strings.Contains(reason, "query embeddings") {
		t.Errorf("instruct reason = %q", reason)
	}

	// The surface embeds text only.
	request = inference.EmbedRequest{
		Items: []inference.EmbedItem{{Content: message.Content{
			Parts: []message.Part{imagePart(t, "https://cdn.example.com/a.png")},
		}}},
	}
	_, report, err = compileEmbedWire(t, declared, request)
	if err == nil {
		t.Fatal("an image item compiled on the text-embedding surface")
	}
	if reason := rejectedReason(report, inference.FieldEmbedItemImage); !strings.Contains(reason, "embeds text") {
		t.Errorf("image item reason = %q", reason)
	}

	// A multi-part item joins in order and says so.
	request = inference.EmbedRequest{
		Items: []inference.EmbedItem{{Content: message.Content{
			Parts: []message.Part{
				message.TextPart{Text: "alpha"},
				message.TextPart{Text: "beta"},
			},
		}}},
	}
	wire, report, err := compileEmbedWire(t, declared, request)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(wire.batches) != 1 || wire.batches[0][0] != "alpha\nbeta" {
		t.Errorf("joined item = %#v", wire.batches)
	}
	decision, ok := decisionFor(report, inference.FieldEmbedItemMultiPart)
	if !ok || decision.Disposition != inference.Dropped {
		t.Errorf("multi-part decision = %+v", decision)
	}

	// Custom sizes bind only where the capability is declared.
	noSizes := declared
	noSizes.Capabilities.CustomEmbedDimensions = false
	request = inference.EmbedRequest{
		Items:      []inference.EmbedItem{embedTextItem("alpha")},
		Dimensions: intPtr(1024),
	}
	_, report, err = compileEmbedWire(t, noSizes, request)
	if err == nil {
		t.Fatal("a model without the capability accepted dimensions")
	}
	if reason := rejectedReason(report, inference.FieldEmbedDimensions); !strings.Contains(reason, "does not declare custom embed dimensions") {
		t.Errorf("capability reason = %q", reason)
	}
}

// TestTransportEmbedBatches drives the batched transport: one request per
// batch, vectors placed by their global index, and the token counts summed.
func TestTransportEmbedBatches(t *testing.T) {
	platform := &fakePlatform{}
	platform.embed = func(body map[string]any) (int, string) {
		if body["model"] != "qwen3.7-text-embedding" {
			t.Errorf("model = %v", body["model"])
		}
		if _, ok := body["parameters"]; ok {
			t.Errorf("an untouched batch carried parameters: %#v", body["parameters"])
		}
		input, ok := body["input"].(map[string]any)
		if !ok {
			t.Fatalf("input = %#v", body["input"])
		}
		texts, ok := input["texts"].([]any)
		if !ok {
			t.Fatalf("texts = %#v", input["texts"])
		}
		raw, _ := json.Marshal(texts)
		switch string(raw) {
		case `["alpha","beta"]`:
			return 200, `{"output":{"embeddings":[` +
				`{"text_index":1,"embedding":[3,4]},` +
				`{"text_index":0,"embedding":[1,2]}]},` +
				`"usage":{"total_tokens":10},"request_id":"req-1"}`
		case `["gamma"]`:
			return 200, `{"output":{"embeddings":[` +
				`{"text_index":0,"embedding":[5,6]}]},` +
				`"usage":{"prompt_tokens":4},"request_id":"req-2"}`
		}
		t.Errorf("unexpected batch %s", raw)
		return 200, "{}"
	}
	declared := testDeclaration(t, "qwen3.7-text-embedding")
	driver, err := openEmbed(
		&clients{media: testClients(t, platform)}, declared,
		model.ModelID{Provider: providerID, Name: declared.Name},
	)
	if err != nil {
		t.Fatalf("openEmbed: %v", err)
	}
	request := inference.EmbedRequest{
		Items: []inference.EmbedItem{
			embedTextItem("alpha"),
			embedTextItem("beta"),
			embedTextItem("gamma"),
		},
	}
	response, err := driver.Execute(
		context.Background(),
		model.ModelRef{ID: model.ModelID{Provider: providerID, Name: declared.Name}},
		request,
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(response.Embeddings) != 3 {
		t.Fatalf("embeddings = %#v", response.Embeddings)
	}
	for index, want := range [][]float32{{1, 2}, {3, 4}, {5, 6}} {
		if !slices.Equal(response.Embeddings[index].Vector, want) {
			t.Errorf("embedding %d = %v, want %v",
				index, response.Embeddings[index].Vector, want)
		}
	}
	if response.Usage.InputTokens != 14 {
		t.Errorf("input tokens = %d", response.Usage.InputTokens)
	}
	if response.Usage.ItemCount != 3 {
		t.Errorf("item count = %d", response.Usage.ItemCount)
	}
	if response.Metadata.RequestID != "req-1" {
		t.Errorf("request id = %q", response.Metadata.RequestID)
	}
	calls := platform.recorded()
	if len(calls) != 2 {
		t.Fatalf("recorded calls = %#v", calls)
	}
	for _, call := range calls {
		if call.path != embedPathText {
			t.Errorf("call path = %q", call.path)
		}
	}
}

// TestTransportEmbedMissingVector pins the fail-closed decode: a batch that
// never fills one of its slots fails instead of yielding a zero vector.
func TestTransportEmbedMissingVector(t *testing.T) {
	platform := &fakePlatform{
		embed: func(map[string]any) (int, string) {
			return 200, `{"output":{"embeddings":[` +
				`{"text_index":0,"embedding":[1,2]}]},` +
				`"usage":{"total_tokens":3},"request_id":"req-3"}`
		},
	}
	declared := testDeclaration(t, "qwen3.7-text-embedding")
	driver, err := openEmbed(
		&clients{media: testClients(t, platform)}, declared,
		model.ModelID{Provider: providerID, Name: declared.Name},
	)
	if err != nil {
		t.Fatalf("openEmbed: %v", err)
	}
	request := inference.EmbedRequest{
		Items: []inference.EmbedItem{embedTextItem("alpha"), embedTextItem("beta")},
	}
	_, err = driver.Execute(
		context.Background(),
		model.ModelRef{ID: model.ModelID{Provider: providerID, Name: declared.Name}},
		request,
	)
	if err == nil {
		t.Fatal("a batch with a missing vector decoded successfully")
	}
	var providerErr *inference.Error
	if !errors.As(err, &providerErr) ||
		providerErr.Kind != inference.InvalidProviderResponse {
		t.Fatalf("error = %v", err)
	}
	if cause := errors.Unwrap(providerErr); cause == nil ||
		!strings.Contains(cause.Error(), "no vector for text 1") {
		t.Errorf("cause = %v", providerErr)
	}
}
