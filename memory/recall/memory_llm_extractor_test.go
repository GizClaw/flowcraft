package recall

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/ingest"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

// scriptedLLM is a minimal llm.LLM for testing the WithLLMExtractor
// facade option. It returns the configured Response on every
// Generate call and records the options it received so tests can
// verify the extractor pipeline wired them correctly.
type scriptedLLM struct {
	mu                    sync.Mutex
	Response              string
	Responses             []string
	ResponsesBySchemaName map[string]string
	Options               [][]llm.GenerateOption
}

func (s *scriptedLLM) Generate(_ context.Context, _ []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Options = append(s.Options, opts)
	body := ""
	if s.ResponsesBySchemaName != nil {
		got := llm.GenerateOptions{}
		for _, opt := range opts {
			opt(&got)
		}
		if got.JSONSchema != nil {
			body = s.ResponsesBySchemaName[got.JSONSchema.Name]
		}
	}
	if len(s.Responses) > 0 {
		body = s.Responses[0]
		s.Responses = s.Responses[1:]
	} else if body == "" {
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

type publicBlockingStageLLM struct {
	stage string
}

func (b publicBlockingStageLLM) Generate(ctx context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if len(msgs) > 0 && msgs[0].Content() == b.stage {
		<-ctx.Done()
		return llm.Message{}, llm.TokenUsage{}, ctx.Err()
	}
	return llm.NewTextMessage(llm.RoleAssistant, `{"facts":[]}`), llm.TokenUsage{}, nil
}

func (b publicBlockingStageLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("publicBlockingStageLLM: streaming not implemented")
}

func TestWithLLMExtractor_WiresExtractorIntoSavePath(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{Response: `{"facts":[{
		"text":"Alice said Paris is her city.",
		"kind":"state",
		"subject":"Alice",
		"predicate":"",
		"object":"",
		"entities":["Alice","Paris"],
		"evidence_refs":[{"id":"D1:3","text":"Alice said Paris is her city."}]
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
	client := &scriptedLLM{ResponsesBySchemaName: map[string]string{
		"recall_facts_v2":            `{"facts":[{"text":"Alice likes Paris.","subject":"Alice","source_ids":["D1:3"],"quote":"Alice likes Paris."}]}`,
		"recall_facts_v2_assertions": `{"facts":[{"text":"Alice likes Paris.","subject":"Alice","polarity":"affirmed","modality":"actual","certainty":"explicit","source_ids":["D1:3"],"quote":"Alice likes Paris."}]}`,
		"recall_facts_v2_kinds":      `{"facts":[{"text":"Alice likes Paris.","kind":"preference","subject":"Alice","source_ids":["D1:3"],"quote":"Alice likes Paris."}]}`,
		"recall_facts_v2_relations":  `{"facts":[{"text":"Alice likes Paris.","subject":"Alice","predicate":"likes","object":"Paris","source_ids":["D1:3"],"quote":"Alice likes Paris."}]}`,
		"recall_facts_v2_entities":   `{"facts":[{"text":"Alice likes Paris.","subject":"Alice","entities":["Alice","Paris"],"source_ids":["D1:3"],"quote":"Alice likes Paris."}]}`,
		"recall_facts_v2_evidence":   `{"links":[{"fact_index":0,"evidence_refs":[{"id":"D1:3","text":"Alice likes Paris."}]}]}`,
	}}
	mem, err := New(
		WithTemporalStore(store),
		WithLLMExtractor(
			client,
			WithLLMExtractionMode(LLMExtractionTwoPass),
			WithLLMExtractorTemperature(0.3),
			WithLLMExtractorSchemaName("recall_facts_v2"),
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
	if len(client.Options) != 6 {
		t.Fatalf("two-pass mode should call five raw field extractors and grounding; got %d", len(client.Options))
	}
	seenSchemas := map[string]bool{}
	for _, opts := range client.Options {
		got := llm.GenerateOptions{}
		for _, opt := range opts {
			opt(&got)
		}
		if got.Temperature == nil || *got.Temperature != 0.3 {
			t.Fatalf("temperature should propagate to every two-pass call, got %v", got.Temperature)
		}
		if got.JSONSchema == nil {
			t.Fatalf("schema missing from two-pass call")
		}
		seenSchemas[got.JSONSchema.Name] = true
	}
	for _, name := range []string{
		"recall_facts_v2",
		"recall_facts_v2_assertions",
		"recall_facts_v2_kinds",
		"recall_facts_v2_relations",
		"recall_facts_v2_entities",
		"recall_facts_v2_evidence",
	} {
		if !seenSchemas[name] {
			t.Fatalf("schema %q not used, saw %+v", name, seenSchemas)
		}
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

func TestWithLLMExtractor_TwoPassStageTimeoutOption(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	client := publicBlockingStageLLM{stage: ingest.TwoPassFactExtractionPrompt}
	mem, err := New(
		WithTemporalStore(store),
		WithLLMExtractor(
			client,
			WithLLMExtractionMode(LLMExtractionTwoPass),
			WithLLMExtractorStageTimeout(20*time.Millisecond),
		),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = mem.Save(ctx, Scope{RuntimeID: "rt", UserID: "u1"}, SaveRequest{
		Turns: []TurnContext{{ID: "D1:3", Role: "user", Text: "Alice likes Paris."}},
	})
	if err == nil {
		t.Fatal("blocked two-pass content stage should fail save")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("save error = %v, want context deadline exceeded", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("outer context should still be live, got %v", ctx.Err())
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("stage timeout should cancel promptly, elapsed=%s", elapsed)
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
