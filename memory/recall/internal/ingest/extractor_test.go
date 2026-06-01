package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

// TestExtractedFactSchema_IsValidStrictJSONSchema pins the wire-shape
// invariants OpenAI's strict structured-output mode enforces server-
// side: every object must list every property in `required` and set
// `additionalProperties: false`. A schema that fails this check
// returns 400 from the provider on the FIRST Save call of a fresh
// deployment — costly to diagnose in production. Catching the
// regression at package-test time is cheap.
func TestExtractedFactSchema_IsValidStrictJSONSchema(t *testing.T) {
	assertStrictJSONSchema(t, "ExtractedFactSchema", ExtractedFactSchema)
}

func TestExtractorSchemas_RequireLLMSemanticAssertionFields(t *testing.T) {
	for _, field := range []string{"polarity", "modality", "certainty"} {
		if !strings.Contains(ExtractedFactSchema, `"`+field+`"`) {
			t.Fatalf("ExtractedFactSchema should expose semantic field %q in the strict LLM contract", field)
		}
	}
	for _, field := range []string{"frame_type", "preferred", "alternative", "dimension", "amount", "unit"} {
		if strings.Contains(ExtractedFactSchema, `"`+field+`"`) {
			t.Fatalf("ExtractedFactSchema should not expose frame field %q in the strict LLM contract", field)
		}
	}
	for _, field := range []string{"source_ids", "quote"} {
		if !strings.Contains(ExtractedFactSchema, `"`+field+`"`) {
			t.Fatalf("ExtractedFactSchema should use compact evidence field %q", field)
		}
	}
	if strings.Contains(ExtractedFactSchema, `"evidence_refs"`) {
		t.Fatalf("ExtractedFactSchema should not force nested evidence_refs in single-pass output")
	}
}

func assertStrictJSONSchema(t *testing.T, name string, schema string) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(schema), &root); err != nil {
		t.Fatalf("%s is not valid JSON: %v", name, err)
	}
	var walk func(path string, node map[string]any)
	walk = func(path string, node map[string]any) {
		if kind, _ := node["type"].(string); kind == "object" {
			props, _ := node["properties"].(map[string]any)
			req, _ := node["required"].([]any)
			if len(props) > 0 && len(req) != len(props) {
				t.Errorf("%s.%s: strict mode requires required==properties keys, got %d vs %d", name, path, len(req), len(props))
			}
			if v, ok := node["additionalProperties"]; !ok || v != false {
				t.Errorf("%s.%s: strict mode requires additionalProperties:false, got %v", name, path, v)
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
	mu                sync.Mutex
	Responses         []string
	ResponsesBySystem map[string][]string
	Usages            []llm.TokenUsage
	UsagesBySystem    map[string][]llm.TokenUsage
	Err               error
	Messages          [][]llm.Message
	Options           [][]llm.GenerateOption
}

func (f *fakeLLM) Generate(_ context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Messages = append(f.Messages, msgs)
	f.Options = append(f.Options, opts)
	if len(msgs) > 0 && f.ResponsesBySystem != nil {
		system := msgs[0].Content()
		if responses := f.ResponsesBySystem[system]; len(responses) > 0 {
			body := responses[0]
			f.ResponsesBySystem[system] = responses[1:]
			return llm.NewTextMessage(llm.RoleAssistant, body), f.nextUsageForSystem(system), nil
		}
	}
	if len(f.Responses) == 0 {
		if f.Err != nil {
			return llm.Message{}, llm.TokenUsage{}, f.Err
		}
		return llm.NewTextMessage(llm.RoleAssistant, `{"facts":[]}`), llm.TokenUsage{}, nil
	}
	body := f.Responses[0]
	f.Responses = f.Responses[1:]
	return llm.NewTextMessage(llm.RoleAssistant, body), f.nextUsage(), nil
}

func (f *fakeLLM) nextUsageForSystem(system string) llm.TokenUsage {
	if f.UsagesBySystem != nil {
		if usages := f.UsagesBySystem[system]; len(usages) > 0 {
			usage := usages[0]
			f.UsagesBySystem[system] = usages[1:]
			return usage
		}
	}
	return f.nextUsage()
}

func (f *fakeLLM) nextUsage() llm.TokenUsage {
	if len(f.Usages) == 0 {
		return llm.TokenUsage{}
	}
	usage := f.Usages[0]
	f.Usages = f.Usages[1:]
	return usage
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
	// a valid tagged input with a JSONL turn so the LLM has something
	// to cite back.
	client := &fakeLLM{
		Responses: []string{`{"facts":[{"text":"Avery likes Riverton.","evidence_refs":[{"id":"turn-1"}]}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{Text: "Avery likes Riverton."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 || out[0].Content != "Avery likes Riverton." {
		t.Errorf("prose-only turn not extracted: %+v", out)
	}
	if userMsg := client.Messages[0][1].Content(); !strings.Contains(userMsg, `"id":"turn-1"`) {
		t.Errorf("synthetic turn id missing from wire shape: %q", userMsg)
	}
}

func TestLLMExtractor_RendersTurnsAsJSONL(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[
			{"text":"Avery prefers blue over red.","evidence_refs":[{"id":"D1:3"}]},
			{"text":"Avery plans to visit Riverton on 2024-05-07.","evidence_refs":[{"id":"D1:5","text":"[2024-05-07] Avery: I'm going to Riverton."}]}
		]}`},
	}
	ex := NewLLMExtractor(client)
	turn1 := port.TurnContext{ID: "D1:3", EvidenceID: "D1:3", Role: "user", Speaker: "Avery", Time: time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC), Text: "Blue is my favorite color, not red."}
	turn2 := port.TurnContext{ID: "D1:5", EvidenceID: "D1:5", Role: "user", Speaker: "Avery", Time: time.Date(2024, 5, 7, 9, 0, 0, 0, time.UTC), Text: "I'm going to Riverton."}
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
	if out[0].Content != "Avery prefers blue over red." {
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
	if out[0].EvidenceText != turn1.Text || out[0].EvidenceRefs[0].Text != turn1.Text {
		t.Errorf("evidence text should use source turn text, got fact=%q ref=%q", out[0].EvidenceText, out[0].EvidenceRefs[0].Text)
	}
	if out[1].EvidenceText != turn2.Text || out[1].EvidenceRefs[0].Text != turn2.Text {
		t.Errorf("non-verbatim evidence quote should fall back to source turn text, got fact=%q ref=%q", out[1].EvidenceText, out[1].EvidenceRefs[0].Text)
	}
	if len(out[0].SourceMessageIDs) != 1 || out[0].SourceMessageIDs[0] != "D1:3" {
		t.Errorf("source ids not derived from evidence: %+v", out[0].SourceMessageIDs)
	}

	// Wire shape: user message must be a tagged envelope with JSONL
	// source turns, not the legacy free-form prose.
	if len(client.Messages) != 1 {
		t.Fatalf("LLM must be called once, got %d", len(client.Messages))
	}
	userMsg := client.Messages[0][1].Content()
	if !strings.Contains(userMsg, "<extractor_input>") || !strings.Contains(userMsg, `<source_turns format="jsonl">`) || !strings.Contains(userMsg, "</source_turns>") {
		t.Errorf("tagged source-turn envelope missing from user message: %q", userMsg)
	}
	if !strings.Contains(userMsg, `"id":"D1:3"`) || !strings.Contains(userMsg, `"speaker":"Avery"`) || !strings.Contains(userMsg, `"time":"2024-05-01T09:00:00Z"`) {
		t.Errorf("typed turn fields missing from JSONL user message: %q", userMsg)
	}
}

func TestLLMExtractor_AcceptsCompactSourceIDsAndQuote(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery joined the North Window book club.",
			"kind":"note",
			"subject":"Avery",
			"predicate":"",
			"object":"",
			"polarity":"affirmed",
			"modality":"actual",
			"certainty":"explicit",
			"entities":["Avery","North Window book club"],
			"source_ids":["D1:3"],
			"quote":"I joined the North Window book club."
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:3",
			Role:    "user",
			Speaker: "Avery",
			Text:    "I joined the North Window book club.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d (%+v)", len(out), out)
	}
	if out[0].Kind != domain.KindNote || out[0].Subject != "Avery" {
		t.Fatalf("compact source_ids/quote fact not preserved: %+v", out[0])
	}
	if len(out[0].EvidenceRefs) != 1 || out[0].EvidenceRefs[0].ID != "D1:3" || out[0].EvidenceRefs[0].Text != "I joined the North Window book club." {
		t.Fatalf("compact source_ids/quote not converted to evidence refs: %+v", out[0].EvidenceRefs)
	}
}

func TestLLMExtractor_DropsFactsGroundedOnlyInRecentContext(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery's dogs love getting lots of exercise.",
			"kind":"preference",
			"subject":"Avery's dogs",
			"predicate":"love",
			"object":"getting lots of exercise",
			"polarity":"affirmed",
			"modality":"actual",
			"certainty":"explicit",
			"entities":["Avery","dogs"],
			"source_ids":["D1:1"],
			"quote":"getting lots of exercise"
		},{
			"text":"Avery's dogs hate snow.",
			"kind":"preference",
			"subject":"Avery's dogs",
			"predicate":"hate",
			"object":"snow",
			"polarity":"affirmed",
			"modality":"actual",
			"certainty":"explicit",
			"entities":["Avery","dogs","snow"],
			"source_ids":["D1:2"],
			"quote":"they hate snow"
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		RecentMessages: []domain.Message{{
			Role: "user",
			Text: "Avery's dogs love getting lots of exercise.",
		}},
		Turns: []port.TurnContext{{
			ID:      "D1:2",
			Role:    "user",
			Speaker: "Avery",
			Text:    "One thing they hate is snow.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want only current-source fact, got %d (%+v)", len(out), out)
	}
	if out[0].Content != "Avery's dogs hate snow." || out[0].EvidenceRefs[0].ID != "D1:2" {
		t.Fatalf("wrong fact survived source guard: %+v", out[0])
	}
}

func TestLLMExtractor_DeduplicatesFactsWithinSave(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery's dogs love running.",
			"kind":"preference",
			"subject":"Avery's dogs",
			"predicate":"love",
			"object":"running",
			"polarity":"affirmed",
			"modality":"actual",
			"certainty":"explicit",
			"entities":["Avery","dogs","running"],
			"source_ids":["D1:3"],
			"quote":"They love running"
		},{
			"text":"Avery's dogs love running.",
			"kind":"preference",
			"subject":"Avery's dogs",
			"predicate":"love",
			"object":"running",
			"polarity":"affirmed",
			"modality":"actual",
			"certainty":"explicit",
			"entities":["Avery","dogs","running"],
			"source_ids":["D1:3"],
			"quote":"They love running"
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:3",
			Role:    "user",
			Speaker: "Avery",
			Text:    "They love running.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want duplicate collapsed to 1 fact, got %d (%+v)", len(out), out)
	}
}

func TestLLMExtractor_PromotesPlanModalityFromEvidence(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery wants to visit Riverton.",
			"kind":"plan",
			"subject":"Avery",
			"predicate":"",
			"object":"",
			"polarity":"affirmed",
			"modality":"actual",
			"certainty":"explicit",
			"entities":["Avery","Riverton"],
			"source_ids":["D1:4"],
			"quote":"I want to visit Riverton."
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:4",
			Role:    "user",
			Speaker: "Avery",
			Text:    "I want to visit Riverton.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d (%+v)", len(out), out)
	}
	if out[0].Modality != domain.ModalityActual {
		t.Fatalf("extractor should not infer plan modality from cue tables, got %q (%+v)", out[0].Modality, out[0])
	}
}

func TestExtractorPromptsForbidGenericSurfaceCollapse(t *testing.T) {
	for name, prompt := range map[string]string{
		"single_pass": LLMExtractorSystemPrompt,
	} {
		for _, want := range []string{
			"Never replace an answer-bearing span with only a category word",
			`The Brass Atlas`,
			`North Window`,
			`my dog Comet`,
			`a pet`,
			`an item`,
		} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("%s prompt missing %q:\n%s", name, want, prompt)
			}
		}
	}
}

func TestLLMExtractorReattachesMissingQuotedEvidenceSurface(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery finished reading a book yesterday.",
			"kind":"event",
			"evidence_refs":[{"id":"D1:1","text":"I finished \"Charlotte's Web\" yesterday."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:1",
			Speaker: "Avery",
			Text:    `I finished "Charlotte's Web" yesterday.`,
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d (%+v)", len(out), out)
	}
	if strings.Contains(out[0].Content, "Exact source phrase") {
		t.Fatalf("quoted evidence surface should not pollute content: %q", out[0].Content)
	}
	phrases, _ := out[0].Metadata[domain.MetaExactSourcePhrases].([]string)
	if len(phrases) != 1 || phrases[0] != "Charlotte's Web" {
		t.Fatalf("missing quoted evidence surface metadata: %+v", out[0].Metadata)
	}
}

func TestLLMExtractor_AcceptsFactsSchema(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery plans to visit Riverton.",
			"kind":"plan",
			"subject":"Avery",
			"predicate":"",
			"object":"",
			"entities":["Avery","Riverton"],
			"evidence_refs":[{"id":"D1:3","text":"Avery says she's going to Riverton."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "D1:3", Role: "user", Text: "Avery says she's going to Riverton."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	if out[0].Content != "Avery plans to visit Riverton." {
		t.Errorf("fact text not lifted: %q", out[0].Content)
	}
	if out[0].Kind != domain.KindPlan {
		t.Errorf("fact kind not lifted: got %q want %q", out[0].Kind, domain.KindPlan)
	}
	if len(out[0].EvidenceRefs) != 1 || out[0].EvidenceRefs[0].ID != "D1:3" {
		t.Errorf("fact evidence not lifted: %+v", out[0].EvidenceRefs)
	}
}

// TestLLMExtractor_PropagatesKindEnum verifies the new 3-field schema
// path: when the LLM picks a kind from the closed enum, the extractor
// surfaces it on the TemporalFact so the Structurizer's keyword
// fallback never overwrites it. This is the load-bearing assertion
// that "route 2" actually wires kind through the pipeline.
func TestLLMExtractor_PropagatesKindEnum(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[
			{"text":"Avery lives in Riverton.","kind":"state","evidence_refs":[{"id":"t1"}]},
			{"text":"Avery plans to visit Harborview in June.","kind":"plan","evidence_refs":[{"id":"t1"}]},
			{"text":"Avery loves black coffee.","kind":"preference","evidence_refs":[{"id":"t1"}]},
			{"text":"When comparing options, Avery wants markdown tables.","kind":"procedure","evidence_refs":[{"id":"t1"}]},
			{"text":"Avery is married to Rowan.","kind":"relation","evidence_refs":[{"id":"t1"}]},
			{"text":"Avery went to the cinema on 2024-05-07.","kind":"event","evidence_refs":[{"id":"t1"}]},
			{"text":"Avery mentioned a new book.","kind":"note","evidence_refs":[{"id":"t1"}]}
		]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "t1", Text: "Avery lives in Riverton. Avery plans to visit Harborview in June. Avery loves black coffee. When comparing options, Avery wants markdown tables. Avery is married to Rowan. Avery went to the cinema on 2024-05-07. Avery mentioned a new book."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	want := []domain.FactKind{
		domain.KindState, domain.KindPlan, domain.KindPreference,
		domain.KindProcedure, domain.KindRelation, domain.KindEvent, domain.KindNote,
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

func TestLLMExtractor_DowngradesUnsupportedAssertionFields(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery adopted a cat.",
			"kind":"event",
			"subject":"Avery",
			"predicate":"adopted",
			"object":"cat",
			"entities":["Avery","cat"],
			"polarity":"negated",
			"modality":"counterfactual",
			"certainty":"uncertain",
			"evidence_refs":[{"id":"t1","text":"Avery adopted a cat."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "t1", Speaker: "Avery", Text: "Avery adopted a cat."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("facts = %+v", out)
	}
	if out[0].Polarity != domain.PolarityAffirmed || out[0].Modality != domain.ModalityActual || out[0].Certainty != domain.CertaintyExplicit {
		t.Fatalf("unsupported assertion fields were not downgraded: %+v", out[0])
	}
}

func TestLLMExtractor_KeepsSupportedAssertionFields(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery did not adopt a cat.",
			"kind":"event",
			"subject":"Avery",
			"predicate":"adopted",
			"object":"cat",
			"entities":["Avery","cat"],
			"polarity":"negated",
			"modality":"actual",
			"certainty":"explicit",
			"evidence_refs":[{"id":"t1","text":"Avery did not adopt a cat."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "t1", Speaker: "Avery", Text: "Avery did not adopt a cat."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("facts = %+v", out)
	}
	if out[0].Polarity != domain.PolarityNegated {
		t.Fatalf("supported negation was downgraded: %+v", out[0])
	}
}

func TestSelfContainedExtractedContent_ReducesRepeatedSubjectMentions(t *testing.T) {
	got, ok := selfContainedExtractedContent("My art is about expressing my experience. It's my way of showing my story.", "Riley")
	if !ok {
		t.Fatal("expected self-contained rewrite")
	}
	if strings.Count(got, "Riley") > 1 {
		t.Fatalf("subject repeated too often: %q", got)
	}
	if !strings.Contains(got, "their experience") || !strings.Contains(got, "their way") {
		t.Fatalf("possessive repeats were not reduced naturally: %q", got)
	}
}

func TestLLMExtractor_PropagatesSubjectAndCleansEntities(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Juno made a model bridge in her workshop.",
			"kind":"event",
			"subject":"Juno",
			"predicate":"made",
			"object":"model bridge",
			"entities":["Juno's","workshop","July","on","model bridge","2023","being","taking","finding"],
			"evidence_refs":[{"id":"D1:7","text":"That model bridge is sturdy! Did Juno make it?"}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:7",
			Role:    "assistant",
			Speaker: "Rhea",
			Text:    "That model bridge is sturdy! Did Juno make it?",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	if out[0].Subject != "Juno" {
		t.Fatalf("subject should come from extractor, not evidence speaker: %+v", out[0])
	}
	if out[0].Predicate != "" || out[0].Object != "" {
		t.Fatalf("predicate/object should be cleared when the predicate token is not directly supported, got %q/%q", out[0].Predicate, out[0].Object)
	}
	wantEntities := []string{"Juno", "workshop", "model bridge"}
	if strings.Join(out[0].Entities, ",") != strings.Join(wantEntities, ",") {
		t.Fatalf("entities = %+v, want %+v", out[0].Entities, wantEntities)
	}
}

func TestLLMExtractor_ReplacesWeakSubjectAndDropsWeakContractionEntities(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Orin listens to jazz while working on puzzles.",
			"kind":"note",
			"subject":"I",
			"predicate":"listens to",
			"object":"jazz",
			"entities":["I'm","I'll","Orin","working on puzzles","writing","jazz"],
			"evidence_refs":[{"id":"D1:1","text":"I'm doing my puzzle sketches, I listen to jazz to relax."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:1",
			Role:    "assistant",
			Speaker: "Orin",
			Text:    "I'm doing my puzzle sketches, I listen to jazz to relax.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	if out[0].Subject != "Orin" {
		t.Fatalf("weak subject should fall back to evidence speaker: %+v", out[0])
	}
	wantEntities := []string{"Orin", "jazz"}
	if strings.Join(out[0].Entities, ",") != strings.Join(wantEntities, ",") {
		t.Fatalf("entities = %+v, want %+v", out[0].Entities, wantEntities)
	}
}

func TestLLMExtractor_DropsUnresolvedPronounSubject(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery likes Riverton.",
			"kind":"preference",
			"subject":"they",
			"entities":["Avery","Riverton"],
			"evidence_refs":[{"id":"D1:1","text":"Avery likes Riverton."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:1",
			Role:    "assistant",
			Speaker: "Rowan",
			Text:    "Avery likes Riverton.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	if out[0].Subject != "" {
		t.Fatalf("unresolved pronoun subject should be dropped, got %+v", out[0])
	}
}

func TestLLMExtractor_ClearsIncompleteRelationAndDropsActionPhraseEntities(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[
			{
				"text":"John and his family are planning to repair a bicycle.",
				"kind":"plan",
				"subject":"John",
				"predicate":"",
				"object":"planning to repair a bicycle",
				"entities":["John","planning to repair a bicycle","bicycle"],
				"evidence_refs":[{"id":"D1:1","text":"We're planning to repair a bicycle."}]
			},
			{
				"text":"John values family support.",
				"kind":"state",
				"subject":"John",
				"predicate":"values_family_support",
				"object":"",
				"entities":["John","family support"],
				"evidence_refs":[{"id":"D1:2","text":"John values family support."}]
			}
		]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{
			{ID: "D1:1", Role: "user", Speaker: "John", Text: "We're planning to repair a bicycle."},
			{ID: "D1:2", Role: "user", Speaker: "John", Text: "John values family support."},
		},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 facts, got %d: %+v", len(out), out)
	}
	for i, fact := range out {
		if fact.Predicate != "" || fact.Object != "" {
			t.Fatalf("fact[%d] incomplete relation should be cleared, got %q/%q", i, fact.Predicate, fact.Object)
		}
	}
	if got := strings.Join(out[0].Entities, ","); got != "John,bicycle" {
		t.Fatalf("action phrase entity should be dropped, got %q", got)
	}
}

// TestLLMExtractor_UnknownKindFallsThrough confirms that an
// unrecognised kind label (older deployment, prompt drift) leaves
// Kind empty so the Structurizer's keyword fallback can still
// classify the fact instead of silently shipping garbage to the
// projections.
func TestLLMExtractor_UnknownKindFallsThrough(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{"text":"Avery lives in Riverton.","kind":"ufo","evidence_refs":[{"id":"t1"}]}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "t1", Text: "Avery lives in Riverton."}},
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
		Responses: []string{`{"facts":[{"text":"x",
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
		Turns: []port.TurnContext{
			{ID: "D1:3", Text: "Same turn quoted once. Same turn quoted again with a different excerpt."},
			{ID: "D1:4", Text: "A different turn."},
		},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	refs := out[0].EvidenceRefs
	if len(refs) != 2 {
		t.Fatalf("evidence refs should dedupe to two valid turn ids, got %d: %+v", len(refs), refs)
	}
	if refs[0].ID != "D1:3" || refs[1].ID != "D1:4" {
		t.Errorf("dedupe must preserve first-occurrence order, got %+v", refs)
	}
}

func TestLLMExtractor_RepairsEvidenceIDFromVerbatimQuote(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery adopted a golden retriever named Waffles.",
			"kind":"event",
			"evidence_refs":[{"id":"D1:1","text":"I adopted a golden retriever named Waffles."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{
			{ID: "D1:1", Text: "That's wonderful news!"},
			{ID: "D1:2", Role: "user", Speaker: "Avery", Text: "I adopted a golden retriever named Waffles."},
		},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	if len(out[0].EvidenceRefs) != 1 || out[0].EvidenceRefs[0].ID != "D1:2" {
		t.Fatalf("evidence id should repair to quoted turn, got %+v", out[0].EvidenceRefs)
	}
	if got := out[0].SourceMessageIDs; len(got) != 1 || got[0] != "D1:2" {
		t.Fatalf("source ids should follow repaired evidence id, got %+v", got)
	}
}

func TestLLMExtractor_SingleTurnEvidenceFallback(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery likes Riverton.",
			"kind":"state",
			"evidence_refs":[]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "D1:3", Role: "user", Text: "Avery likes Riverton."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	if len(out[0].EvidenceRefs) != 1 || out[0].EvidenceRefs[0].ID != "D1:3" {
		t.Fatalf("single-turn fallback should attach evidence id, got %+v", out[0].EvidenceRefs)
	}
}

func TestLLMExtractor_DropsUngroundedMultiTurnMemory(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery likes Riverton.",
			"kind":"state",
			"subject":"Avery",
			"entities":["Avery","Riverton"],
			"evidence_refs":[]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{
			{ID: "D1:1", Role: "user", Text: "Avery likes Riverton."},
			{ID: "D1:2", Role: "assistant", Text: "Nice."},
		},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("ungrounded multi-turn memory should be dropped, got %+v", out)
	}
}

func TestLLMExtractor_DropsNamedEntityUnsupportedByEvidence(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Eli visited Riverton yesterday.",
			"kind":"event",
			"subject":"Eli",
			"entities":["jon","riverton"],
			"evidence_refs":[{"id":"D1:1","text":"I'm on the hunt for the ideal spot for my dance studio."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:1",
			Speaker: "Eli",
			Text:    "I'm on the hunt for the ideal spot for my dance studio.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("named entity absent from evidence should drop fact, got %+v", out)
	}
}

func TestLLMExtractor_DropsUnsupportedContentAnchorsWhenFieldsEmpty(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Eli visited Riverton yesterday.",
			"kind":"event",
			"subject":"",
			"predicate":"",
			"object":"",
			"entities":[],
			"evidence_refs":[{"id":"D1:1","text":"Avery stayed home yesterday."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:1",
			Speaker: "Avery",
			Text:    "Avery stayed home yesterday.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("unsupported title-case content anchors should drop fact, got %+v", out)
	}
}

func TestLLMExtractor_DropsNumericMismatchUnsupportedByEvidence(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery bought 3 books yesterday.",
			"kind":"event",
			"subject":"Avery",
			"predicate":"",
			"object":"",
			"entities":["Avery","books"],
			"evidence_refs":[{"id":"D1:1","text":"I bought 2 books yesterday."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:1",
			Speaker: "Avery",
			Text:    "I bought 2 books yesterday.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("numeric mismatch should drop fact, got %+v", out)
	}
}

func TestLLMExtractor_AllowsResolvedRelativeDateNotLiteralInEvidence(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"On 2026-05-19, Mira went to a woodworking meetup.",
			"kind":"event",
			"subject":"Mira",
			"predicate":"attended",
			"object":"woodworking meetup",
			"entities":["Mira","woodworking meetup"],
			"evidence_refs":[{"id":"D1:3","text":"I went to a woodworking meetup yesterday and it was inspiring."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:3",
			Speaker: "Mira",
			Time:    time.Date(2026, 5, 20, 13, 56, 0, 0, time.UTC),
			Text:    "I went to a woodworking meetup yesterday and it was inspiring.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("resolved relative date should not be treated as unsupported numeric hallucination, got %+v", out)
	}
	if out[0].Predicate != "" || out[0].Object != "" {
		t.Fatalf("relation should be cleared without predicate-token support, got %q/%q", out[0].Predicate, out[0].Object)
	}
}

func TestLLMExtractor_RewritesFirstPersonContentWhenSubjectResolved(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"I went to a woodworking meetup yesterday.",
			"kind":"event",
			"subject":"Mira",
			"predicate":"attended",
			"object":"woodworking meetup",
			"entities":["Mira","woodworking meetup"],
			"evidence_refs":[{"id":"D1:3","text":"I went to a woodworking meetup yesterday."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:3",
			Speaker: "Mira",
			Text:    "I went to a woodworking meetup yesterday.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want rewritten fact, got %+v", out)
	}
	if out[0].Content != "Mira went to a woodworking meetup yesterday." {
		t.Fatalf("content = %q", out[0].Content)
	}
}

func TestLLMExtractor_RewritesLowercasePossessiveContentWhenSubjectResolved(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"my apartment lost power.",
			"kind":"event",
			"subject":"James",
			"entities":["James","apartment"],
			"evidence_refs":[{"id":"D1:3","text":"my apartment lost power."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "D1:3", Speaker: "James", Text: "my apartment lost power."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want rewritten fact, got %+v", out)
	}
	if out[0].Content != "James's apartment lost power." {
		t.Fatalf("content = %q", out[0].Content)
	}
}

func TestLLMExtractor_RewritesEmbeddedFirstPersonContentWhenSubjectResolved(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Woodworking helps me express my emotions.",
			"kind":"state",
			"subject":"Mira",
			"entities":["Mira","woodworking"],
			"evidence_refs":[{"id":"D1:3","text":"Woodworking helps me express my emotions."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "D1:3", Speaker: "Mira", Text: "Woodworking helps me express my emotions."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want rewritten fact, got %+v", out)
	}
	if out[0].Content != "Woodworking helps Mira express Mira's emotions." {
		t.Fatalf("content = %q", out[0].Content)
	}
}

func TestLLMExtractor_DropsUnresolvedEmbeddedPluralContent(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Taking care of ourselves is important.",
			"kind":"state",
			"subject":"Mira",
			"entities":["Mira"],
			"evidence_refs":[{"id":"D1:3","text":"Taking care of ourselves is important."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "D1:3", Speaker: "Mira", Text: "Taking care of ourselves is important."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("plural first-person residue should drop, got %+v", out)
	}
}

func TestLLMExtractor_DropsNonSelfContainedPluralContent(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"We went to a woodworking meetup yesterday.",
			"kind":"event",
			"subject":"Mira",
			"entities":["Mira","woodworking meetup"],
			"evidence_refs":[{"id":"D1:3","text":"We went to a woodworking meetup yesterday."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "D1:3", Speaker: "Mira", Text: "We went to a woodworking meetup yesterday."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("non-self-contained plural content should drop, got %+v", out)
	}
}

func TestLLMExtractor_DropsTrivialEllipsisContent(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"...",
			"kind":"note",
			"subject":"",
			"entities":[],
			"evidence_refs":[{"id":"D1:3","text":"..."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "D1:3", Speaker: "Mira", Text: "..."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("trivial ellipsis content should drop, got %+v", out)
	}
}

func TestLLMExtractor_DropsEvidenceRefWithUnknownID(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Eli visited Riverton yesterday.",
			"kind":"event",
			"subject":"Eli",
			"entities":["Eli","Riverton"],
			"evidence_refs":[{"id":"D1:404","text":"Eli visited Riverton yesterday."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:1",
			Speaker: "Avery",
			Text:    "Avery stayed home yesterday.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("unknown evidence id should be dropped before support check, got %+v", out)
	}
}

func TestLLMExtractor_SuppressedWeakSubjectSurvivesStructurizer(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"text":"Avery likes Riverton.",
			"kind":"preference",
			"subject":"they",
			"entities":["Avery","Riverton"],
			"evidence_refs":[{"id":"D1:1","text":"Avery likes Riverton."}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	turn := port.TurnContext{ID: "D1:1", Role: "assistant", Speaker: "Rowan", Text: "Avery likes Riverton."}
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{turn},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %+v", out)
	}
	if out[0].Subject != "" {
		t.Fatalf("weak unresolved subject should be suppressed, got %q", out[0].Subject)
	}
	if suppressed, _ := out[0].Metadata[domain.MetaSubjectSuppressed].(bool); !suppressed {
		t.Fatalf("subject suppression metadata missing: %+v", out[0].Metadata)
	}
	structured := DefaultStructurizer{}.Structurize(out[0], port.IngestInput{Turns: []port.TurnContext{turn}})
	if structured.Subject != "" {
		t.Fatalf("structurizer should not fill suppressed subject from speaker, got %q", structured.Subject)
	}
}

func TestLLMExtractor_DuplicateSourceTurnIDFailsValidation(t *testing.T) {
	client := &fakeLLM{Err: errors.New("must not be called")}
	ex := NewLLMExtractor(client)
	_, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{
			{ID: "D1:1", Text: "Avery likes Riverton."},
			{ID: "D1:1", Text: "Avery moved to Harborview."},
		},
	})
	if err == nil {
		t.Fatal("duplicate source turn id should fail validation")
	}
	if !strings.Contains(err.Error(), "duplicate source turn id") {
		t.Fatalf("duplicate id error = %v", err)
	}
	if len(client.Messages) != 0 {
		t.Fatalf("LLM should not be called for invalid source turns, got %d calls", len(client.Messages))
	}
}

func TestLLMExtractor_UncoveredTurnDoesNotTriggerSecondLLMCall(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{
			`{"facts":[{"text":"Avery likes Riverton.","kind":"preference","subject":"Avery","predicate":"","object":"","entities":["Avery","Riverton"],"evidence_refs":[{"id":"D1:1","text":"Avery likes Riverton."}]}]}`,
		},
		Err: errors.New("unexpected second call"),
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{
			{ID: "D1:1", Role: "user", Speaker: "Avery", Text: "Avery likes Riverton."},
			{ID: "D1:2", Role: "user", Speaker: "Avery", Text: "I bought 2 wooden figurines yesterday for my family."},
		},
	})
	if err != nil {
		t.Fatalf("extract should not make a second LLM call: %v", err)
	}
	if len(out) != 1 || out[0].Content != "Avery likes Riverton." {
		t.Fatalf("extract should keep primary facts, got %+v", out)
	}
}

func TestLLMExtractor_HandlesFencedJSON(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{"Sure, here is the result:\n```json\n{\"facts\":[{\"text\":\"hello\",\"evidence_refs\":[]}]}\n```\n"},
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
// actions ("events") from durable traits ("states"). Regression
// analysis traced time-anchored recall misses to the extractor
// over-summarising sentences like "I just signed up for a class
// yesterday" into "<speaker> uses classes for self-expression" —
// collapsing several dated events into abstract states. Future prompt
// edits must keep:
//   - explicit instruction to default past-tense+date snippets to
//     kind:"event" (not state/preference);
//   - explicit instruction to preserve single-mention proper nouns
//     verbatim (book titles, locations, items);
//   - explicit exhaustiveness for one-off mentions;
//   - explicit enumeration splitting, so comma-separated hobbies /
//     activities do not collapse into an umbrella summary;
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
		"1. Extraction strategy",
		"2. Candidate policy",
		"3. Preserve answer-bearing detail",
		"4. Avoid abstraction and over-merge",
		"5. Evidence grounding",
		"6. Text and subject fields",
		"7. Relation fields",
		"8. Semantic assertion fields",
		"9. Second-person comments",
		"10. Entity anchors",
		"11. Kind taxonomy",
		"12. Source ids and quotes",
		"13. Coverage examples",
		"14. Coverage checklist",
		"15. Empty result",
		`NOT {kind:"state"`,
		"Single-occurrence dated\n                     actions are events, not states",
		"Work source-turn by source-turn",
		"Do not stop after the first event in a turn",
		`Treat "note" as a first-class memory kind`,
		"Be exhaustive about concrete, retrievable details",
		"Split enumerations into separate facts",
		"PersonA enjoys birdwatching",
		"Do not\n  collapse lists into",
		"Preserve literal answer-bearing spans",
		"that book",
		"Be careful with second-person comments",
		"second-person detail is about the addressee",
		"do not leave first-person or group pronouns anywhere",
		"dialogue act instead of memory\n  content",
		"questions, requests for updates",
		"subject -> predicate -> object",
		"MUST be filled as a pair",
		"Never emit an object without a predicate",
		"Prefer canonical predicates only when their meaning exactly",
		"short snake_case source verb",
		"map an unsupported relation to the nearest canonical predicate",
		"owns_pet is only for named animals",
		"recommended requires an explicit recommendation",
		"likes/enjoys/prefers require",
		"semantic annotations, not keyword labels",
		"They annotate the FACT PROPOSITION",
		`"unknown" only when`,
		"desired future visit",
		`If a sentence is only about a current feeling toward a future thing`,
		"Do not invent an object",
		"symbolic meanings, abstract outcomes",
		"planning to fix a garden cart",
		`"procedure"`,
		"When comparing options, use a markdown\n                     table.",
		"Quote proper nouns verbatim",
		"The Brass Atlas",
		"<source_turns>",
		"Ground each fact in the DIRECT source turn",
		"Do not cite neighbouring turns",
		"Do not create cross-turn summary facts",
		"Use multiple source_ids only when one fact truly\n  requires both turns together",
		"Never cite ids from <recent_context>",
		"Prefer quoting the exact words that make the fact true",
		"names\n  of groups, clubs, organisations",
		"reasons, outcomes,\n  lessons learned",
		`emit it as kind "note" instead of dropping it`,
		"Use note for named group details",
		"Morning Lanterns",
		"Lake Merrow",
		"A typical memorable turn\n  may produce 2-6 facts",
		"Resolve relative dates against the source turn's timestamp",
		`For kind "plan", do not use "actual"`,
		`Do not leave relative-time words as the main time anchor`,
		`"in July 2023" when only the month is knowable`,
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
