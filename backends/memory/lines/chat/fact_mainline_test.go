package chat

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

func TestFactStrategyDefaultsAndValidation(t *testing.T) {
	config := DefaultConfig()
	if config.Strategy != StrategySimple || config.TailMaxChars != 15000 {
		t.Fatalf("defaults = %#v", config)
	}
	for _, strategy := range []FactStrategy{StrategyNone, StrategySimple, StrategyRich} {
		config.Strategy = strategy
		if err := config.Validate(); err != nil {
			t.Fatalf("%s: %v", strategy, err)
		}
	}
	config.Strategy = "invalid"
	if err := config.Validate(); err == nil {
		t.Fatal("invalid strategy accepted")
	}
	config = DefaultConfig()
	config.MaxFacts = 0
	if err := config.Validate(); err == nil {
		t.Fatal("zero max facts accepted")
	}
}

func TestFactExtractorNoneSkipsAndSimpleBatchesOnceWithRuneTail(t *testing.T) {
	model := inferencetest.DefaultFakeModel
	noneFake := &inferencetest.GenerateFake{Respond: jsonResponse(`{"facts":[]}`)}
	config := DefaultConfig()
	config.Strategy = StrategyNone
	config.Runtime = noneFake.Assembly(t)
	config.GenerateModel = &model
	extractor, err := NewFactExtractorWithConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := extractor.Derive(context.Background(), rawMessageArtifact()); err != nil || len(got) != 0 {
		t.Fatalf("none = %#v, %v", got, err)
	}
	if len(noneFake.Requests()) != 0 {
		t.Fatal("none called generate")
	}

	fake := &inferencetest.GenerateFake{Respond: jsonResponse(`{"facts":[
		{"text":"ALICE　likes  Tea","entities":[" Alice ","TEA"]},
		{"text":"alice likes tea","entities":["alice"]},
		{"text":"Bob lives in Paris","entities":["Bob","Paris"]}
	]}`)}
	config = DefaultConfig()
	config.Runtime = fake.Assembly(t)
	config.GenerateModel = &model
	config.TailMaxChars = 4
	extractor, err = NewFactExtractorWithConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	input := rawMessageArtifact()
	input.Content = coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "前文🙂甲乙丙丁"}}}
	got, err := extractor.Derive(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.Requests()) != 1 || len(got) != 2 {
		t.Fatalf("calls=%d facts=%d", len(fake.Requests()), len(got))
	}
	if prompt := fake.LastRequest().Input.Content.Text(); !strings.HasSuffix(prompt, "甲乙丙丁") || strings.Contains(prompt, "🙂") {
		t.Fatalf("tail prompt = %q", prompt)
	}
	if got[0].ID == "" || got[0].Metadata["canonical_hash"] == "" ||
		got[0].Metadata["transform_signature"] != TransformSignatureSimple {
		t.Fatalf("fact metadata = %#v", got[0].Metadata)
	}
}

func TestFactExtractorRichSchemaMalformedAndCaps(t *testing.T) {
	model := inferencetest.DefaultFakeModel
	fake := &inferencetest.GenerateFake{Respond: jsonResponse(`{"facts":[{"text":"Alice works at Acme","entities":["Alice","Acme"],"predicate":"works_at","temporal_detail":"since 2020"}]}`)}
	config := DefaultConfig()
	config.Strategy = StrategyRich
	config.Runtime = fake.Assembly(t)
	config.GenerateModel = &model
	extractor, err := NewFactExtractorWithConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	got, err := extractor.Derive(context.Background(), rawMessageArtifact())
	if err != nil || len(got) != 1 || got[0].Metadata["predicate"] != "works_at" {
		t.Fatalf("rich = %#v, %v", got, err)
	}
	schema := string(fake.LastRequest().Input.Content.Intent.Text.Response.Schema)
	if !strings.Contains(schema, `"predicate"`) || !strings.Contains(schema, `"additionalProperties":false`) {
		t.Fatalf("rich schema = %s", schema)
	}

	for name, response := range map[string]string{
		"unknown":  `{"facts":[{"text":"ok","unknown":true}]}`,
		"not_json": `no facts here`,
	} {
		t.Run(name, func(t *testing.T) {
			local := DefaultConfig()
			local.Runtime = (&inferencetest.GenerateFake{Respond: jsonResponse(response)}).Assembly(t)
			local.GenerateModel = &model
			local.MaxFacts = 1
			local.MaxFactChars = 4
			extractor, err := NewFactExtractorWithConfig(local)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := extractor.Derive(context.Background(), rawMessageArtifact()); err == nil {
				t.Fatal("invalid model output accepted")
			}
		})
	}
	// Oversized output is truncated instead of failing the commit, so a
	// chatty generation cannot poison the stream.
	for name, expect := range map[string]struct {
		response string
		want     string
	}{
		"too_many": {`{"facts":[{"text":"one"},{"text":"two"}]}`, "one"},
		"too_long": {`{"facts":[{"text":"12345"}]}`, "1234"},
	} {
		t.Run(name, func(t *testing.T) {
			local := DefaultConfig()
			local.Runtime = (&inferencetest.GenerateFake{Respond: jsonResponse(expect.response)}).Assembly(t)
			local.GenerateModel = &model
			local.MaxFacts = 1
			local.MaxFactChars = 4
			extractor, err := NewFactExtractorWithConfig(local)
			if err != nil {
				t.Fatal(err)
			}
			got, err := extractor.Derive(context.Background(), rawMessageArtifact())
			if err != nil || len(got) != 1 || got[0].Content.Text() != expect.want {
				t.Fatalf("truncated derive = %#v, %v", got, err)
			}
		})
	}
}

// TestDecodeFactsAcceptsFencedAndProseWrappedJSON covers providers that wrap
// the payload in a Markdown fence or prose despite the prompt contract; both
// the strict (schema) and lenient (json_object) decoders must recover it.
func TestDecodeFactsAcceptsFencedAndProseWrappedJSON(t *testing.T) {
	for name, response := range map[string]string{
		"fenced":   "```json\n{\"facts\":[{\"text\":\"Alice likes tea\"}]}\n```",
		"prose":    "Here are the facts:\n{\"facts\":[{\"text\":\"Alice likes tea\"}]}\nHope that helps!",
		"trailing": "{\"facts\":[{\"text\":\"Alice likes tea\"}]}\n\nNote: extracted from the transcript.",
		"array":    "```\n[{\"text\":\"Alice likes tea\"}]\n```",
	} {
		t.Run(name, func(t *testing.T) {
			loose, err := decodeFacts([]byte(response), false)
			if err != nil || len(loose.Facts) != 1 || loose.Facts[0].Text != "Alice likes tea" {
				t.Fatalf("lenient decode = %#v, %v", loose, err)
			}
			// The strict path only accepts the schema shape; the array
			// wrapper is not a schema response, so it stays a decode error.
			if name == "array" {
				return
			}
			strict, err := decodeFacts([]byte(response), true)
			if err != nil || len(strict.Facts) != 1 {
				t.Fatalf("strict decode = %#v, %v", strict, err)
			}
		})
	}
}

func TestCanonicalFactHashIsStableAndVersioned(t *testing.T) {
	left := CanonicalFactHash(" Café\tLIKES　Tea ")
	right := CanonicalFactHash("cafe\u0301 likes tea")
	if left != right || !strings.HasPrefix(left, CanonicalAlgorithmVersion+":") {
		t.Fatalf("hashes = %q, %q", left, right)
	}
	if left == CanonicalFactHash("café likes coffee") {
		t.Fatal("different content collided")
	}
}

func TestFactExtractorLinksEntityEmbeddingTopFiveAndTimeWindow(t *testing.T) {
	ctx := context.Background()
	runtime, generate, embed, embedding := associationRuntime(t)
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	logStore, err := storage.NewWorkspaceLog(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	store, err := factview.NewFactStore(logStore, kvStore)
	if err != nil {
		t.Fatal(err)
	}
	eventTime := time.Date(2026, 8, 5, 2, 0, 0, 0, time.UTC)
	items := []struct {
		id       string
		text     string
		entities []string
		at       time.Time
		links    []string
	}{
		{"f", "entity candidate", []string{"Alice"}, eventTime.Add(-time.Hour), nil},
		{"g", "time candidate", nil, eventTime.Add(5 * time.Minute), nil},
		{factID(CanonicalFactHash("existing duplicate")), "existing duplicate", []string{"legacy"}, eventTime, []string{"legacy-link"}},
	}
	for index := 0; index < 297; index++ {
		id := fmt.Sprintf("existing-%03d", index)
		items = append(items, struct {
			id       string
			text     string
			entities []string
			at       time.Time
			links    []string
		}{id: id, text: "candidate " + id, at: eventTime.Add(-time.Hour)})
	}
	for _, item := range items {
		_, err := store.Add(ctx, factview.AddRequest{
			ID: item.id, Scope: scope, ConversationID: "conversation",
			Content:  coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: item.text}}},
			Entities: item.entities, EventTime: item.at,
			LinkedMemoryIDs: item.links,
			Provenance:      []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: item.id}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	config := DefaultConfig()
	config.Runtime, config.GenerateModel, config.EmbedModel, config.Facts = runtime, &generate, &embed, store
	searcher := &recordingVectorSearcher{results: []component.Candidate{
		{ID: "e"}, {ID: "d"}, {ID: "c"}, {ID: "b"}, {ID: "a"},
	}}
	config.LinkVectorSearcher = searcher
	extractor, err := NewFactExtractorWithConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	input := rawMessageArtifact()
	input.Metadata["runtime_id"] = scope.RuntimeID
	input.Metadata["user_id"] = scope.UserID
	input.Metadata["conversation_id"] = "conversation"
	input.Metadata["event_time"] = eventTime.Format(time.RFC3339Nano)
	got, err := extractor.Derive(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("facts=%d", len(got))
	}
	if embedding.calls != 1 || !reflect.DeepEqual(embedding.items, []string{"new fact", "batch peer"}) {
		t.Fatalf("embedding calls=%d items=%v", embedding.calls, embedding.items)
	}
	t.Logf("existing=300 output=%d embedded=%d", len(got), len(embedding.items))
	if len(searcher.requests) != 2 {
		t.Fatalf("vector searches = %d", len(searcher.requests))
	}
	duplicateLinks := decodeStrings(got[0].Metadata["linked_memory_ids"])
	if !containsString(duplicateLinks, "legacy-link") {
		t.Fatalf("duplicate did not inherit links: %v", duplicateLinks)
	}
	merged, err := store.Add(ctx, factview.AddRequest{
		ID: got[0].ID, CanonicalHash: got[0].Metadata["canonical_hash"],
		Scope: scope, ConversationID: "conversation", Content: got[0].Content,
		Entities:  decodeStrings(got[0].Metadata["entities"]),
		EventTime: eventTime, LinkedMemoryIDs: duplicateLinks, Provenance: got[0].Sources,
		SourceDigest: got[0].Metadata["source_digest"], TransformSignature: got[0].Metadata["transform_signature"],
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(merged.Entities, "alice") || !containsString(merged.Entities, "legacy") ||
		!containsString(merged.LinkedMemoryIDs, "legacy-link") || len(merged.Provenance) != 3 {
		t.Fatalf("duplicate merge = %#v", merged)
	}
}

func TestFactExtractorEmbeddingLinksGracefullySkipMissingProjectionAndPropagateFailures(t *testing.T) {
	for _, test := range []struct {
		name      string
		searchErr error
		embedFail bool
		wantErr   bool
	}{
		{name: "projection missing", searchErr: errdefs.NotFoundf("projection missing")},
		{name: "search failure", searchErr: errors.New("projection read failed"), wantErr: true},
		{name: "embed failure", embedFail: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, generate, embed, embedding := associationRuntime(t)
			embedding.fail = test.embedFail
			config := DefaultConfig()
			config.Runtime, config.GenerateModel, config.EmbedModel = runtime, &generate, &embed
			config.LinkVectorSearcher = &recordingVectorSearcher{err: test.searchErr}
			extractor, err := NewFactExtractorWithConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			got, err := extractor.Derive(context.Background(), addressedRawMessageArtifact())
			if (err != nil) != test.wantErr {
				t.Fatalf("facts=%v err=%v", got, err)
			}
			if !test.wantErr && (len(got) != 3 ||
				!containsString(decodeStrings(got[0].Metadata["linked_memory_ids"]), got[1].ID) ||
				!containsString(decodeStrings(got[0].Metadata["linked_memory_ids"]), got[2].ID)) {
				t.Fatalf("missing projection batch links = %#v", got)
			}
		})
	}
}

type associationEmbedding struct {
	calls int
	items []string
	fail  bool
}

type associationEmbedWire struct{ Texts []string }

func associationRuntime(t *testing.T) (*inference.Assembly, model.ModelRef, model.ModelRef, *associationEmbedding) {
	t.Helper()
	generate := model.ModelRef{ID: model.ModelID{Provider: "association", Name: "generate"}}
	embed := model.ModelRef{ID: model.ModelID{Provider: "association", Name: "embed"}}
	embedding := &associationEmbedding{}
	generateDriver, err := inference.BindGenerate(
		func(_ context.Context, _ model.ModelRef, request inference.GenerateRequest, shape inference.GenerateExecutionShape) (inference.Compiled[string], error) {
			return inference.Compiled[string]{Wire: "wire", Report: inferencetest.NativeReport(model.OperationGenerate, request.ActiveFieldsFor(shape)...)}, nil
		},
		func(context.Context, string) (string, error) {
			return `{"facts":[{"text":"existing duplicate","entities":["Alice"]},{"text":"new fact"},{"text":"batch peer"}]}`, nil
		},
		func(_ context.Context, raw string) (inference.GenerateResponse, error) {
			return jsonResponse(raw)(inference.GenerateRequest{}), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	embedDriver, err := inference.BindEmbed(
		func(_ context.Context, _ model.ModelRef, request inference.EmbedRequest) (inference.Compiled[associationEmbedWire], error) {
			texts := make([]string, len(request.Items))
			for index := range request.Items {
				texts[index] = request.Items[index].Content.Text()
			}
			return inference.Compiled[associationEmbedWire]{
				Wire:   associationEmbedWire{Texts: texts},
				Report: inferencetest.NativeReport(model.OperationEmbed, request.ActiveFields()...),
			}, nil
		},
		func(_ context.Context, wire associationEmbedWire) ([][]float32, error) {
			embedding.calls++
			embedding.items = append([]string(nil), wire.Texts...)
			if embedding.fail {
				return nil, errors.New("embed unavailable")
			}
			vectors := make([][]float32, len(wire.Texts))
			for index := range vectors {
				vectors[index] = []float32{1, 1}
			}
			return vectors, nil
		},
		func(_ context.Context, vectors [][]float32) (inference.EmbedResponse, error) {
			result := inference.EmbedResponse{Embeddings: make([]inference.Embedding, len(vectors))}
			for index := range vectors {
				result.Embeddings[index].Vector = vectors[index]
			}
			return result, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t, []inference.ProviderDefinition{{
		ID: generate.ID.Provider,
		Models: []inference.ModelImplementation{
			{Descriptor: model.ModelDescriptor{ID: generate.ID}, Openers: inference.Openers{
				Generate: func(context.Context, model.ModelRef) (inference.GenerateOperations, error) {
					return inference.GenerateOperations{Unary: generateDriver}, nil
				},
			}},
			{Descriptor: model.ModelDescriptor{ID: embed.ID}, Openers: inference.Openers{
				Embed: func(context.Context, model.ModelRef) (inference.EmbedDriver, error) {
					return embedDriver, nil
				},
			}},
		},
	}})
	return runtime, generate, embed, embedding
}

type recordingVectorSearcher struct {
	requests []component.VectorSearchRequest
	results  []component.Candidate
	err      error
}

func (searcher *recordingVectorSearcher) SearchVector(_ context.Context, request component.VectorSearchRequest) ([]component.Candidate, error) {
	searcher.requests = append(searcher.requests, request)
	return append([]component.Candidate(nil), searcher.results...), searcher.err
}

func addressedRawMessageArtifact() component.Artifact {
	input := rawMessageArtifact()
	input.Metadata["runtime_id"] = "runtime"
	input.Metadata["user_id"] = "user"
	input.Metadata["conversation_id"] = "conversation"
	input.Metadata["event_time"] = time.Date(2026, 8, 5, 2, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	return input
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
