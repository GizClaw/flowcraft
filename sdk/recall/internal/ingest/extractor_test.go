package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// TestExtractedFactSchema_IsValidStrictJSONSchema pins the wire-shape
// invariants OpenAI's strict structured-output mode enforces server-
// side: every object must list every property in `required` and set
// `additionalProperties: false`. A schema that fails this check
// returns 400 from the provider on the FIRST Save call of a fresh
// deployment — costly to diagnose in production. Catching the
// regression at package-test time is cheap.
func TestExtractedFactSchema_IsValidStrictJSONSchema(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal([]byte(ExtractedFactSchema), &root); err != nil {
		t.Fatalf("ExtractedFactSchema is not valid JSON: %v", err)
	}
	var walk func(path string, node map[string]any)
	walk = func(path string, node map[string]any) {
		if kind, _ := node["type"].(string); kind == "object" {
			props, _ := node["properties"].(map[string]any)
			req, _ := node["required"].([]any)
			if len(props) > 0 && len(req) != len(props) {
				t.Errorf("%s: strict mode requires required==properties keys, got %d vs %d", path, len(req), len(props))
			}
			if v, ok := node["additionalProperties"]; !ok || v != false {
				t.Errorf("%s: strict mode requires additionalProperties:false, got %v", path, v)
			}
			for name, raw := range props {
				if child, ok := raw.(map[string]any); ok {
					walk(path+"."+name, child)
				}
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(path+"[]", items)
		}
	}
	walk("root", root)
}

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
		return llm.NewTextMessage(llm.RoleAssistant, `{"memories":[]}`), llm.TokenUsage{}, nil
	}
	body := f.Responses[0]
	f.Responses = f.Responses[1:]
	return llm.NewTextMessage(llm.RoleAssistant, body), llm.TokenUsage{}, nil
}

func (f *fakeLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("fakeLLM: streaming not implemented")
}

func TestLLMExtractor_EmptyInputSkipsLLM(t *testing.T) {
	client := &fakeLLM{Err: errors.New("must not be called")}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Facts: []domain.TemporalFact{{Kind: domain.KindNote, Content: "structured"}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 || out[0].Content != "structured" {
		t.Errorf("structured facts must pass through, got %+v", out)
	}
	if len(client.Messages) != 0 {
		t.Errorf("LLM should not be called when Turns is empty, calls=%d", len(client.Messages))
	}
}

func TestLLMExtractor_ProseTurnSynthesizesID(t *testing.T) {
	// Callers without per-turn metadata pass a single port.TurnContext
	// with only Text populated; the extractor must still produce
	// a valid JSONL line so the LLM has something to cite back.
	client := &fakeLLM{
		Responses: []string{`{"memories":[{"text":"Alice likes Paris.","evidence_refs":[{"id":"turn-1"}]}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{Text: "Alice likes Paris."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 || out[0].Content != "Alice likes Paris." {
		t.Errorf("prose-only turn not extracted: %+v", out)
	}
	if userMsg := client.Messages[0][1].Content(); !strings.Contains(userMsg, `"id":"turn-1"`) {
		t.Errorf("synthetic turn id missing from wire shape: %q", userMsg)
	}
}

func TestLLMExtractor_RendersTurnsAsJSONL(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"memories":[
			{"text":"Alice prefers blue over red.","evidence_refs":[{"id":"D1:3"}]},
			{"text":"Alice plans to visit Paris on 2024-05-07.","evidence_refs":[{"id":"D1:5","text":"[2024-05-07] Alice: I'm going to Paris."}]}
		]}`},
	}
	ex := NewLLMExtractor(client)
	turn1 := port.TurnContext{ID: "D1:3", EvidenceID: "D1:3", Role: "user", Speaker: "Alice", Time: time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC), Text: "Blue is my favorite color, not red."}
	turn2 := port.TurnContext{ID: "D1:5", EvidenceID: "D1:5", Role: "user", Speaker: "Alice", Time: time.Date(2024, 5, 7, 9, 0, 0, 0, time.UTC), Text: "I'm going to Paris."}
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{turn1, turn2},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 facts, got %d (%+v)", len(out), out)
	}
	if out[0].Content != "Alice prefers blue over red." {
		t.Errorf("content not preserved: %q", out[0].Content)
	}
	if len(out[0].EvidenceRefs) != 1 || out[0].EvidenceRefs[0].ID != "D1:3" {
		t.Errorf("evidence_refs not preserved: %+v", out[0].EvidenceRefs)
	}
	// Typed turn metadata must be lifted into the EvidenceRef so
	// downstream materializers see Role/Timestamp without parsing.
	if out[0].EvidenceRefs[0].Role != "user" || out[0].EvidenceRefs[0].Timestamp.IsZero() {
		t.Errorf("evidence ref should inherit typed turn metadata, got %+v", out[0].EvidenceRefs[0])
	}
	if len(out[0].SourceMessageIDs) != 1 || out[0].SourceMessageIDs[0] != "D1:3" {
		t.Errorf("source ids not derived from evidence: %+v", out[0].SourceMessageIDs)
	}

	// Wire shape: user message must be JSONL with the typed turn
	// fields, not the legacy free-form prose.
	if len(client.Messages) != 1 {
		t.Fatalf("LLM must be called once, got %d", len(client.Messages))
	}
	userMsg := client.Messages[0][1].Content()
	if !strings.Contains(userMsg, `"id":"D1:3"`) || !strings.Contains(userMsg, `"speaker":"Alice"`) || !strings.Contains(userMsg, `"time":"2024-05-01T09:00:00Z"`) {
		t.Errorf("typed turn fields missing from JSONL user message: %q", userMsg)
	}
}

func TestLLMExtractor_AcceptsLegacyFactsSchema(t *testing.T) {
	// A deployment may still have the older prompt cached; make
	// sure the parser tolerates the legacy "facts" envelope and
	// projects content + kind + evidence_refs.id into the new shape.
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"kind":"plan","content":"Alice plans to visit Paris.",
			"evidence_refs":[{"id":"D1:3","message_id":"D1:3","role":"user","text":"Alice says she's going to Paris."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "D1:3", Role: "user", Text: "Alice says she's going to Paris."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	if out[0].Content != "Alice plans to visit Paris." {
		t.Errorf("legacy content not lifted: %q", out[0].Content)
	}
	if out[0].Kind != domain.KindPlan {
		t.Errorf("legacy kind not lifted: got %q want %q", out[0].Kind, domain.KindPlan)
	}
	if len(out[0].EvidenceRefs) != 1 || out[0].EvidenceRefs[0].ID != "D1:3" {
		t.Errorf("legacy evidence not lifted: %+v", out[0].EvidenceRefs)
	}
}

// TestLLMExtractor_PropagatesKindEnum verifies the new 3-field schema
// path: when the LLM picks a kind from the closed enum, the extractor
// surfaces it on the TemporalFact so the Structurizer's keyword
// fallback never overwrites it. This is the load-bearing assertion
// that "route 2" actually wires kind through the pipeline.
func TestLLMExtractor_PropagatesKindEnum(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"memories":[
			{"text":"Alice lives in Paris.","kind":"state","evidence_refs":[{"id":"t1"}]},
			{"text":"Alice plans to visit Berlin in June.","kind":"plan","evidence_refs":[{"id":"t1"}]},
			{"text":"Alice loves black coffee.","kind":"preference","evidence_refs":[{"id":"t1"}]},
			{"text":"Alice is married to Bob.","kind":"relation","evidence_refs":[{"id":"t1"}]},
			{"text":"Alice went to the cinema on 2024-05-07.","kind":"event","evidence_refs":[{"id":"t1"}]},
			{"text":"Alice mentioned a new book.","kind":"note","evidence_refs":[{"id":"t1"}]}
		]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "t1", Text: "irrelevant — schema is what we check here"}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	want := []domain.FactKind{
		domain.KindState, domain.KindPlan, domain.KindPreference,
		domain.KindRelation, domain.KindEvent, domain.KindNote,
	}
	if len(out) != len(want) {
		t.Fatalf("want %d facts, got %d", len(want), len(out))
	}
	for i, w := range want {
		if out[i].Kind != w {
			t.Errorf("fact[%d].Kind = %q, want %q (content=%q)", i, out[i].Kind, w, out[i].Content)
		}
	}
}

// TestLLMExtractor_UnknownKindFallsThrough confirms that an
// unrecognised kind label (older deployment, prompt drift) leaves
// Kind empty so the Structurizer's keyword fallback can still
// classify the fact instead of silently shipping garbage to the
// projections.
func TestLLMExtractor_UnknownKindFallsThrough(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"memories":[{"text":"Alice lives in Paris.","kind":"ufo","evidence_refs":[{"id":"t1"}]}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "t1", Text: "x"}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	if out[0].Kind != "" {
		t.Errorf("unknown kind should fall through to empty for Structurizer fallback, got %q", out[0].Kind)
	}
}

func TestLLMExtractor_DedupesEvidenceRefs(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"memories":[{"text":"x",
			"evidence_refs":[
				{"id":"D1:3","text":"Same turn quoted once."},
				{"id":"D1:3","text":"Same turn quoted again with a different excerpt."},
				{"id":"D1:4","text":"A different turn."},
				{"id":"","text":"Same turn quoted again with a different excerpt."},
				{"id":"D1:4","text":"A different turn."}
			]}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "D1:3", Text: "anything"}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	refs := out[0].EvidenceRefs
	if len(refs) != 3 {
		t.Fatalf("evidence refs should dedupe to 3 (D1:3 + D1:4 + textual variant), got %d: %+v", len(refs), refs)
	}
	if refs[0].ID != "D1:3" || refs[1].ID != "D1:4" {
		t.Errorf("dedupe must preserve first-occurrence order, got %+v", refs)
	}
}

func TestLLMExtractor_HandlesFencedJSON(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{"Sure, here is the result:\n```json\n{\"memories\":[{\"text\":\"hello\",\"evidence_refs\":[]}]}\n```\n"},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "t1", Text: "anything"}},
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
	_, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "t1", Text: "anything"}},
	})
	if err == nil {
		t.Fatal("expected LLM error to surface")
	}
}

func TestLLMExtractor_RejectsMalformedJSON(t *testing.T) {
	client := &fakeLLM{Responses: []string{"{not json"}}
	ex := NewLLMExtractor(client)
	_, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "t1", Text: "anything"}},
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
	_, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "t1", Text: "anything"}},
	})
	if err == nil {
		t.Fatal("expected backend error to surface")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Errorf("backend NotAvailable classification lost: %v", err)
	}
}

// TestLLMExtractorSystemPrompt_GuardsAntiAbstraction pins the
// anti-abstraction language that distinguishes one-off dated
// actions ("events") from durable traits ("states"). LoCoMo
// failure analysis (2026-05-21) traced ~half of recall misses to
// the extractor over-summarising sentences like "I just signed up
// for pottery yesterday" into "<speaker> uses pottery for self-
// expression" — collapsing five dated events into two abstract
// states. Future prompt edits must keep:
//   - explicit instruction to default past-tense+date snippets to
//     kind:"event" (not state/preference);
//   - explicit instruction to preserve single-mention proper nouns
//     verbatim (book titles, locations, items);
//   - explicit exhaustiveness for one-off mentions;
//
// otherwise we silently regress recall on time-anchored memory.
//
// The test is intentionally substring-based (not whitespace-
// sensitive) so re-wrapping the prompt is free, but removing a
// guarded clause requires deleting the corresponding assertion
// and forces the reviewer to acknowledge the trade-off.
func TestLLMExtractorSystemPrompt_GuardsAntiAbstraction(t *testing.T) {
	mustContain := []string{
		"signed up for a pottery class",
		"NOT {kind:\"state\"",
		"Single-occurrence dated\n                     actions are events, not states",
		"Be exhaustive about concrete, retrievable details",
		"Quote proper nouns verbatim",
		"Charlotte's Web",
	}
	for _, s := range mustContain {
		if !strings.Contains(LLMExtractorSystemPrompt, s) {
			t.Errorf("LLMExtractorSystemPrompt missing anti-abstraction guard: %q", s)
		}
	}
}

func TestStaticExtractor_ReturnsClones(t *testing.T) {
	ex := StaticExtractor{Facts: []domain.TemporalFact{{
		Kind:     domain.KindNote,
		Content:  "x",
		Entities: []string{"a"},
	}}}
	out, err := ex.Extract(context.Background(), port.IngestInput{})
	if err != nil {
		t.Fatal(err)
	}
	out[0].Entities[0] = "mutated"
	out2, _ := ex.Extract(context.Background(), port.IngestInput{})
	if out2[0].Entities[0] != "a" {
		t.Errorf("StaticExtractor must clone facts, got %v", out2[0].Entities)
	}
}
