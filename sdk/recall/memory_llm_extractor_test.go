package recall

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/ingest"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

// scriptedLLM is a minimal llm.LLM for testing the WithLLMExtractor
// facade option. It returns the configured Response on every
// Generate call and records the options it received so tests can
// verify the extractor pipeline wired them correctly.
type scriptedLLM struct {
	Response  string
	Responses []string
	Options   [][]llm.GenerateOption
}

func (s *scriptedLLM) Generate(_ context.Context, _ []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	s.Options = append(s.Options, opts)
	body := ""
	if len(s.Responses) > 0 {
		body = s.Responses[0]
		s.Responses = s.Responses[1:]
	} else {
		body = s.Response
	}
	if body == "" {
		body = `{"facts":[]}`
	}
	return llm.NewTextMessage(llm.RoleAssistant, body), llm.TokenUsage{}, nil
}

func (s *scriptedLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("scriptedLLM: streaming not implemented")
}

func TestWithLLMExtractor_WiresExtractorIntoSavePath(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{Response: `{"facts":[{
		"kind":"preference",
		"subject":"alice",
		"predicate":"city",
		"content":"Paris",
		"source_message_ids":["D1:3"],
		"evidence_refs":[{"id":"D1:3","message_id":"m-3","role":"user","text":"Alice said Paris is her city.","timestamp":"2026-05-19T05:00:00Z"}]
	}]}`}

	mem, err := New(
		WithTemporalStore(store),
		WithLLMExtractor(
			client,
			WithLLMExtractorTemperature(0.2),
			WithLLMExtractorSchemaName("recall_facts_v1"),
		),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Turns: []TurnContext{{ID: "D1:3", Role: "user", Text: "Alice said Paris is her city."}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 1 {
		t.Fatalf("save returned %d ids", len(res.FactIDs))
	}
	fact, err := store.Get(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if len(fact.EvidenceRefs) != 1 || fact.EvidenceRefs[0].ID != "D1:3" {
		t.Fatalf("LLM-extracted evidence refs not persisted: %+v", fact.EvidenceRefs)
	}
	if len(fact.SourceMessageIDs) != 1 || fact.SourceMessageIDs[0] != "D1:3" {
		t.Fatalf("source message ids not persisted: %+v", fact.SourceMessageIDs)
	}
	if len(client.Options) == 0 {
		t.Fatal("expected at least one LLM call to record options")
	}
	last := client.Options[len(client.Options)-1]
	got := llm.GenerateOptions{}
	for _, opt := range last {
		opt(&got)
	}
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Errorf("temperature option not propagated, got=%v", got.Temperature)
	}
	if got.JSONSchema == nil || got.JSONSchema.Name != "recall_facts_v1" {
		t.Errorf("schema name option not propagated, got=%+v", got.JSONSchema)
	}
	if got.JSONMode == nil || !*got.JSONMode {
		t.Errorf("JSON mode should be enabled")
	}
}

func TestWithLLMExtractor_TwoPassModeWiresTwoStepExtractor(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{Responses: []string{
		`{"memories":[{"text":"Alice likes Paris.","kind":"preference"}]}`,
		`{"links":[{"memory_index":0,"evidence_refs":[{"id":"D1:3","text":"Alice likes Paris."}]}]}`,
	}}
	mem, err := New(
		WithTemporalStore(store),
		WithLLMExtractor(
			client,
			WithLLMExtractionMode(LLMExtractionTwoPass),
			WithLLMExtractorTemperature(0.3),
			WithLLMExtractorSchemaName("recall_memories_v2"),
		),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Turns: []TurnContext{{ID: "D1:3", Role: "user", Text: "Alice likes Paris."}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(client.Options) != 2 {
		t.Fatalf("two-pass mode should call LLM twice, got %d", len(client.Options))
	}
	first := llm.GenerateOptions{}
	for _, opt := range client.Options[0] {
		opt(&first)
	}
	second := llm.GenerateOptions{}
	for _, opt := range client.Options[1] {
		opt(&second)
	}
	if first.Temperature == nil || *first.Temperature != 0.3 || second.Temperature == nil || *second.Temperature != 0.3 {
		t.Fatalf("temperature should propagate to both passes, got %v/%v", first.Temperature, second.Temperature)
	}
	if first.JSONSchema == nil || first.JSONSchema.Name != "recall_memories_v2" {
		t.Fatalf("memory schema name not propagated: %+v", first.JSONSchema)
	}
	if second.JSONSchema == nil || second.JSONSchema.Name != "recall_memories_v2_evidence" {
		t.Fatalf("evidence schema name not derived: %+v", second.JSONSchema)
	}
	if len(res.FactIDs) != 1 {
		t.Fatalf("save returned %d ids", len(res.FactIDs))
	}
	fact, err := store.Get(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if fact.Content != "Alice likes Paris." || fact.Kind != FactPreference {
		t.Fatalf("persisted fact content/kind = %q/%q", fact.Content, fact.Kind)
	}
	if len(fact.EvidenceRefs) != 1 || fact.EvidenceRefs[0].ID != "D1:3" {
		t.Fatalf("two-pass evidence refs not persisted: %+v", fact.EvidenceRefs)
	}
}

func TestWithLLMExtractor_IgnoredWhenCompilerProvided(t *testing.T) {
	client := &scriptedLLM{}
	customCompiler := ingest.Default()

	mem, err := New(
		withCompiler(customCompiler),
		WithLLMExtractor(client),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// With both options set, the custom compiler wins. Sanity:
	// we should be able to use Memory without panic. We don't
	// expose the compiler externally so the only check we can
	// make is that LLM was never invoked through this path.
	scope := Scope{RuntimeID: "rt"}
	_, err = mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "noop"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(client.Options) != 0 {
		t.Errorf("LLM extractor must be ignored when a compiler is supplied, calls=%d", len(client.Options))
	}
}

func TestWithLLMExtractor_NilClientFallsBackToPassthrough(t *testing.T) {
	mem, err := New(WithLLMExtractor(nil))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope := Scope{RuntimeID: "rt"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "still works"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 1 {
		t.Errorf("nil LLM client should not break the default compiler, got %d ids", len(res.FactIDs))
	}
}
