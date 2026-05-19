package compiler

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// fakeLLM is a minimal llm.LLM satisfier for tests. It returns
// Responses in order; once exhausted Err (when set) is surfaced.
// Each Generate call records the messages and options received so
// tests can assert prompt + schema wiring.
type fakeLLM struct {
	Responses []string
	Err       error
	Messages  [][]llm.Message
	Options   [][]llm.GenerateOption
}

func (f *fakeLLM) Generate(_ context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	f.Messages = append(f.Messages, msgs)
	f.Options = append(f.Options, opts)
	if len(f.Responses) == 0 {
		if f.Err != nil {
			return llm.Message{}, llm.TokenUsage{}, f.Err
		}
		return llm.NewTextMessage(llm.RoleAssistant, `{"facts":[]}`), llm.TokenUsage{}, nil
	}
	body := f.Responses[0]
	f.Responses = f.Responses[1:]
	return llm.NewTextMessage(llm.RoleAssistant, body), llm.TokenUsage{}, nil
}

func (f *fakeLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("fakeLLM: streaming not implemented")
}

func TestLLMExtractor_EmptyTextSkipsLLM(t *testing.T) {
	client := &fakeLLM{Err: errors.New("must not be called")}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), Input{
		Scope: model.Scope{RuntimeID: "rt"},
		Facts: []model.TemporalFact{{Kind: model.KindNote, Content: "structured"}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 || out[0].Content != "structured" {
		t.Errorf("structured facts must pass through, got %+v", out)
	}
	if len(client.Messages) != 0 {
		t.Errorf("LLM should not be called when text is empty, calls=%d", len(client.Messages))
	}
}

func TestLLMExtractor_ParsesJSONIntoTemporalFacts(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[
			{"kind":"preference","subject":"alice","predicate":"favorite_color","content":"blue","confidence":0.9},
			{"kind":"plan","content":"visit Paris","valid_from_hint":"tomorrow"}
		]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), Input{
		Scope: model.Scope{RuntimeID: "rt"},
		Text:  "Alice says her favourite colour is blue and she's going to Paris tomorrow.",
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 facts, got %d (%+v)", len(out), out)
	}
	if out[0].Kind != model.KindPreference || out[0].Subject != "alice" || out[0].Content != "blue" {
		t.Errorf("first fact = %+v", out[0])
	}
	if out[1].Kind != model.KindPlan {
		t.Errorf("second fact kind = %q", out[1].Kind)
	}
	if hint, _ := out[1].Metadata[MetaValidFromHint].(string); hint != "tomorrow" {
		t.Errorf("valid_from_hint should land in metadata: %v", out[1].Metadata)
	}

	// Assert prompt + schema wiring.
	if len(client.Messages) != 1 {
		t.Fatalf("client must be called once, got %d", len(client.Messages))
	}
	msgs := client.Messages[0]
	if len(msgs) != 2 || msgs[0].Role != llm.RoleSystem || msgs[1].Role != llm.RoleUser {
		t.Errorf("messages = %+v", msgs)
	}
	if msgs[0].Content() != LLMExtractorSystemPrompt {
		t.Errorf("system prompt mismatch: %q", msgs[0].Content())
	}
	gotOpts := llm.GenerateOptions{}
	for _, opt := range client.Options[0] {
		opt(&gotOpts)
	}
	if gotOpts.JSONSchema == nil || gotOpts.JSONSchema.Name != "recall_extracted_facts" || !gotOpts.JSONSchema.Strict {
		t.Errorf("JSON schema option not wired: %+v", gotOpts.JSONSchema)
	}
	if gotOpts.JSONMode == nil || !*gotOpts.JSONMode {
		t.Errorf("JSON mode not enabled")
	}
}

func TestLLMExtractor_HandlesFencedJSON(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{"Sure, here is the result:\n```json\n{\"facts\":[{\"kind\":\"note\",\"content\":\"hello\"}]}\n```\n"},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), Input{
		Scope: model.Scope{RuntimeID: "rt"},
		Text:  "anything",
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 || out[0].Content != "hello" {
		t.Errorf("fenced JSON not parsed, out=%+v", out)
	}
}

func TestLLMExtractor_PropagatesLLMError(t *testing.T) {
	client := &fakeLLM{Err: errors.New("rate limited")}
	ex := NewLLMExtractor(client)
	_, err := ex.Extract(context.Background(), Input{
		Scope: model.Scope{RuntimeID: "rt"},
		Text:  "anything",
	})
	if err == nil {
		t.Fatal("expected LLM error to surface")
	}
}

func TestLLMExtractor_RejectsMalformedJSON(t *testing.T) {
	client := &fakeLLM{Responses: []string{"{not json"}}
	ex := NewLLMExtractor(client)
	_, err := ex.Extract(context.Background(), Input{
		Scope: model.Scope{RuntimeID: "rt"},
		Text:  "anything",
	})
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
	// Malformed model output is a Validation failure at the
	// public boundary — distinguishes a contract-broken reply
	// from a transient backend outage (NotAvailable / Timeout).
	if !errdefs.IsValidation(err) {
		t.Errorf("malformed LLM JSON should map to Validation: %v", err)
	}
}

func TestLLMExtractor_PreservesBackendClassification(t *testing.T) {
	// Backend already classifies as NotAvailable — the extractor
	// must wrap with %w and NOT downgrade the classification.
	backend := errdefs.NotAvailablef("llm: provider down")
	client := &fakeLLM{Err: backend}
	ex := NewLLMExtractor(client)
	_, err := ex.Extract(context.Background(), Input{
		Scope: model.Scope{RuntimeID: "rt"},
		Text:  "anything",
	})
	if err == nil {
		t.Fatal("expected backend error to surface")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Errorf("backend NotAvailable classification lost: %v", err)
	}
}

func TestStaticExtractor_ReturnsClones(t *testing.T) {
	ex := StaticExtractor{Facts: []model.TemporalFact{{
		Kind:     model.KindNote,
		Content:  "x",
		Entities: []string{"a"},
	}}}
	out, err := ex.Extract(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	out[0].Entities[0] = "mutated"
	out2, _ := ex.Extract(context.Background(), Input{})
	if out2[0].Entities[0] != "a" {
		t.Errorf("StaticExtractor must clone facts, got %v", out2[0].Entities)
	}
}
