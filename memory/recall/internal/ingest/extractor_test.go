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
	assertStrictJSONSchema(t, "TwoPassFactExtractionSchema", TwoPassFactExtractionSchema)
	assertStrictJSONSchema(t, "TwoPassKindExtractionSchema", TwoPassKindExtractionSchema)
	assertStrictJSONSchema(t, "TwoPassRelationExtractionSchema", TwoPassRelationExtractionSchema)
	assertStrictJSONSchema(t, "TwoPassEntityExtractionSchema", TwoPassEntityExtractionSchema)
	assertStrictJSONSchema(t, "TwoPassEvidenceGroundingSchema", TwoPassEvidenceGroundingSchema)
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
			return llm.NewTextMessage(llm.RoleAssistant, body), llm.TokenUsage{}, nil
		}
	}
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

func fakeTwoPassLLM(content, kind, relation, entity, evidence []string) *fakeLLM {
	return &fakeLLM{ResponsesBySystem: map[string][]string{
		TwoPassFactExtractionPrompt:     content,
		TwoPassKindExtractionPrompt:     kind,
		TwoPassRelationExtractionPrompt: relation,
		TwoPassEntityExtractionPrompt:   entity,
		TwoPassEvidenceGroundingPrompt:  evidence,
	}}
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
		Responses: []string{`{"memories":[{"text":"Avery likes Riverton.","evidence_refs":[{"id":"turn-1"}]}]}`},
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
		Responses: []string{`{"memories":[
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

func TestExtractorPromptsForbidGenericSurfaceCollapse(t *testing.T) {
	for name, prompt := range map[string]string{
		"single_pass": LLMExtractorSystemPrompt,
		"two_pass":    TwoPassFactExtractionPrompt,
	} {
		for _, want := range []string{
			"Never replace an answer-bearing span with only a category word",
			`The Glass Compass`,
			`Moon Orchard`,
			`my dog Pixel`,
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
		Responses: []string{`{"memories":[{
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
	if !strings.Contains(out[0].Content, `Exact source phrase: "Charlotte's Web".`) {
		t.Fatalf("missing quoted evidence surface in content: %q", out[0].Content)
	}
}

func TestTwoPassLLMExtractorReattachesMissingQuotedEvidenceSurface(t *testing.T) {
	client := fakeTwoPassLLM(
		[]string{`{"facts":[{"text":"Avery finished reading a book yesterday.","subject":"Avery","source_ids":["D1:1"],"quote":"I finished \"Charlotte's Web\" yesterday."}]}`},
		[]string{`{"facts":[{"text":"Avery finished reading a book yesterday.","kind":"event","subject":"Avery","source_ids":["D1:1"],"quote":"I finished \"Charlotte's Web\" yesterday."}]}`},
		[]string{`{"facts":[]}`},
		[]string{`{"facts":[{"text":"Avery finished reading a book yesterday.","subject":"Avery","entities":["Avery","Charlotte's Web"],"source_ids":["D1:1"],"quote":"I finished \"Charlotte's Web\" yesterday."}]}`},
		[]string{`{"links":[{"fact_index":0,"evidence_refs":[{"id":"D1:1","text":"I finished \"Charlotte's Web\" yesterday."}]}]}`},
	)
	ex := NewTwoPassLLMExtractor(client)
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
	if !strings.Contains(out[0].Content, `Exact source phrase: "Charlotte's Web".`) {
		t.Fatalf("missing quoted evidence surface in content: %q", out[0].Content)
	}
}

func TestLLMExtractor_AcceptsLegacyFactsSchema(t *testing.T) {
	// A deployment may still have the older prompt cached; make
	// sure the parser tolerates the legacy "facts" envelope and
	// projects content + kind + evidence_refs.id into the new shape.
	client := &fakeLLM{
		Responses: []string{`{"facts":[{
			"kind":"plan","content":"Avery plans to visit Riverton.",
			"evidence_refs":[{"id":"D1:3","message_id":"D1:3","role":"user","text":"Avery says she's going to Riverton."}]
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
		Turns: []port.TurnContext{{ID: "t1", Text: "irrelevant — schema is what we check here"}},
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

func TestLLMExtractor_PropagatesSubjectAndCleansEntities(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"memories":[{
			"text":"Juno made a bowl in her pottery class.",
			"kind":"event",
			"subject":"Juno",
			"predicate":"made",
			"object":"bowl",
			"entities":["Juno's","pottery","July","on","bowl","2023","being","taking","finding"],
			"evidence_refs":[{"id":"D1:7","text":"That bowl is gorgeous! Did Juno make it?"}]
		}]}`},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID:      "D1:7",
			Role:    "assistant",
			Speaker: "Rhea",
			Text:    "That bowl is gorgeous! Did Juno make it?",
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
	if out[0].Predicate != "made" || out[0].Object != "bowl" {
		t.Fatalf("predicate/object = %q/%q, want made/bowl", out[0].Predicate, out[0].Object)
	}
	wantEntities := []string{"Juno", "pottery", "bowl"}
	if strings.Join(out[0].Entities, ",") != strings.Join(wantEntities, ",") {
		t.Fatalf("entities = %+v, want %+v", out[0].Entities, wantEntities)
	}
}

func TestLLMExtractor_ReplacesWeakSubjectAndDropsWeakContractionEntities(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"memories":[{
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
		Responses: []string{`{"memories":[{
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
		Responses: []string{`{"memories":[
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
		Responses: []string{`{"memories":[{"text":"Avery lives in Riverton.","kind":"ufo","evidence_refs":[{"id":"t1"}]}]}`},
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

func TestLLMExtractor_RepairsEvidenceIDFromVerbatimQuote(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{`{"memories":[{
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
			{ID: "D1:2", Role: "user", Text: "I adopted a golden retriever named Waffles."},
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
		Responses: []string{`{"memories":[{
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
		Responses: []string{`{"memories":[{
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
		Responses: []string{`{"memories":[{
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

func TestLLMExtractor_BackfillsHighSignalUncoveredTurns(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{
			`{"memories":[{"text":"Avery likes Riverton.","kind":"preference","evidence_refs":[{"id":"D1:1"}]}]}`,
			`{"memories":[{"text":"Avery bought 2 ceramic figurines yesterday for her family.","kind":"event","evidence_refs":[{"id":"D1:2","text":"I bought 2 ceramic figurines yesterday"}]}]}`,
		},
	}
	ex := NewLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{
			{ID: "D1:1", Role: "user", Speaker: "Avery", Text: "Avery likes Riverton."},
			{ID: "D1:2", Role: "user", Speaker: "Avery", Text: "I bought 2 ceramic figurines yesterday for my family."},
			{ID: "D1:3", Role: "assistant", Speaker: "Rowan", Text: "Hey Avery, how are you doing?"},
		},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want original memory plus one coverage repair fact, got %d: %+v", len(out), out)
	}
	got := out[1]
	if got.Content != "Avery bought 2 ceramic figurines yesterday for her family." {
		t.Fatalf("repair content = %q", got.Content)
	}
	if len(got.EvidenceRefs) != 1 || got.EvidenceRefs[0].ID != "D1:2" {
		t.Fatalf("repair evidence refs = %+v, want D1:2", got.EvidenceRefs)
	}
	if len(client.Messages) != 2 {
		t.Fatalf("single-pass repair should run one targeted extra extraction, got %d calls", len(client.Messages))
	}
	repairPrompt := client.Messages[1][1].Content()
	if !strings.Contains(repairPrompt, `"id":"D1:2"`) || strings.Contains(repairPrompt, `"id":"D1:3"`) {
		t.Fatalf("repair prompt should include only high-signal uncovered turns, got %q", repairPrompt)
	}
}

func TestTwoPassLLMExtractor_ExtractsThenGroundsEvidence(t *testing.T) {
	client := fakeTwoPassLLM(
		[]string{`{"facts":[{"text":"Avery likes Riverton.","subject":"Avery","source_ids":["D1:3"],"quote":"Avery likes Riverton."}]}`},
		[]string{`{"facts":[{"text":"Avery likes Riverton.","kind":"preference","subject":"Avery","source_ids":["D1:3"],"quote":"Avery likes Riverton."}]}`},
		[]string{`{"facts":[{"text":"Avery likes Riverton.","subject":"Avery","predicate":"likes","object":"Riverton","source_ids":["D1:3"],"quote":"Avery likes Riverton."}]}`},
		[]string{`{"facts":[{"text":"Avery likes Riverton.","subject":"Avery","entities":["Avery","Riverton"],"source_ids":["D1:3"],"quote":"Avery likes Riverton."}]}`},
		[]string{`{"links":[{"fact_index":0,"evidence_refs":[{"id":"D1:3","text":"Avery likes Riverton."}]}]}`},
	)
	ex := NewTwoPassLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{
			{ID: "D1:3", Role: "user", Speaker: "Avery", Text: "Avery likes Riverton."},
			{ID: "D1:4", Role: "assistant", Speaker: "Noah", Text: "That sounds memorable."},
		},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(client.Messages) != 5 {
		t.Fatalf("two-pass extractor should call four raw field extractors plus grounding; got %d", len(client.Messages))
	}
	seenSystems := map[string]bool{}
	var groundingPrompt string
	for _, msgs := range client.Messages {
		if len(msgs) > 0 {
			seenSystems[msgs[0].Content()] = true
		}
		if len(msgs) >= 2 && msgs[0].Content() == TwoPassEvidenceGroundingPrompt {
			groundingPrompt = msgs[1].Content()
		}
	}
	for _, system := range []string{TwoPassFactExtractionPrompt, TwoPassKindExtractionPrompt, TwoPassRelationExtractionPrompt, TwoPassEntityExtractionPrompt} {
		if !seenSystems[system] {
			t.Fatalf("raw field extractor system prompt was not used")
		}
	}
	if groundingPrompt == "" {
		t.Fatalf("default two-pass evidence prompt should run after candidate extraction")
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	if out[0].Content != "Avery likes Riverton." || out[0].Kind != domain.KindPreference {
		t.Fatalf("fact content/kind = %q/%q", out[0].Content, out[0].Kind)
	}
	if out[0].Subject != "Avery" || strings.Join(out[0].Entities, ",") != "Avery,Riverton" {
		t.Fatalf("subject/entities = %q/%+v", out[0].Subject, out[0].Entities)
	}
	if out[0].Predicate != "likes" || out[0].Object != "Riverton" {
		t.Fatalf("predicate/object = %q/%q, want likes/Riverton", out[0].Predicate, out[0].Object)
	}
	if len(out[0].EvidenceRefs) != 1 || out[0].EvidenceRefs[0].ID != "D1:3" {
		t.Fatalf("evidence refs = %+v, want D1:3", out[0].EvidenceRefs)
	}
	if !strings.Contains(groundingPrompt, `"index":0`) || !strings.Contains(groundingPrompt, `"kind":"preference"`) {
		t.Fatalf("grounding prompt should include merged indexed facts, got %q", groundingPrompt)
	}
	if !strings.Contains(groundingPrompt, "<grounding_input>") || !strings.Contains(groundingPrompt, `<facts format="json">`) {
		t.Fatalf("grounding prompt should use tagged input sections, got %q", groundingPrompt)
	}
}

func TestTwoPassLLMExtractor_MergePrefersStrongSubject(t *testing.T) {
	client := fakeTwoPassLLM(
		[]string{`{"facts":[{"text":"Avery likes Riverton.","subject":"they","source_ids":["D1:3"],"quote":"Avery likes Riverton."}]}`},
		[]string{`{"facts":[{"text":"Avery likes Riverton.","kind":"preference","subject":"Avery","source_ids":["D1:3"],"quote":"Avery likes Riverton."}]}`},
		[]string{`{"facts":[]}`},
		[]string{`{"facts":[{"text":"Avery likes Riverton.","subject":"Avery","entities":["Avery","Riverton"],"source_ids":["D1:3"],"quote":"Avery likes Riverton."}]}`},
		[]string{`{"links":[{"fact_index":0,"evidence_refs":[{"id":"D1:3","text":"Avery likes Riverton."}]}]}`},
	)
	ex := NewTwoPassLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "D1:3", Role: "assistant", Speaker: "Rowan", Text: "Avery likes Riverton."}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 fact, got %d", len(out))
	}
	if out[0].Subject != "Avery" {
		t.Fatalf("merged fact should prefer strong subject from other passes, got %+v", out[0])
	}
}

func TestTwoPassLLMExtractor_SingleTurnFallbackWhenGroundingOmitsLink(t *testing.T) {
	client := fakeTwoPassLLM(
		[]string{`{"facts":[{"text":"Avery lives in Riverton.","subject":"Avery","source_ids":["D1:3"],"quote":"Avery lives in Riverton."}]}`},
		[]string{`{"facts":[{"text":"Avery lives in Riverton.","kind":"state","subject":"Avery","source_ids":["D1:3"],"quote":"Avery lives in Riverton."}]}`},
		[]string{`{"facts":[{"text":"Avery lives in Riverton.","subject":"Avery","predicate":"lives_in","object":"Riverton","source_ids":["D1:3"],"quote":"Avery lives in Riverton."}]}`},
		[]string{`{"facts":[{"text":"Avery lives in Riverton.","subject":"Avery","entities":["Avery","Riverton"],"source_ids":["D1:3"],"quote":"Avery lives in Riverton."}]}`},
		[]string{`{"links":[]}`},
	)
	ex := NewTwoPassLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "D1:3", Role: "user", Text: "Avery lives in Riverton."}},
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

func TestTwoPassLLMExtractor_DropsUngroundedMultiTurnMemory(t *testing.T) {
	client := fakeTwoPassLLM(
		[]string{`{"facts":[{"text":"Avery lives in Riverton.","subject":"Avery","source_ids":["D1:1"],"quote":"Avery is in Riverton."}]}`},
		[]string{`{"facts":[{"text":"Avery lives in Riverton.","kind":"state","subject":"Avery","source_ids":["D1:1"],"quote":"Avery is in Riverton."}]}`},
		[]string{`{"facts":[{"text":"Avery lives in Riverton.","subject":"Avery","predicate":"lives_in","object":"Riverton","source_ids":["D1:1"],"quote":"Avery is in Riverton."}]}`},
		[]string{`{"facts":[{"text":"Avery lives in Riverton.","subject":"Avery","entities":["Avery","Riverton"],"source_ids":["D1:1"],"quote":"Avery is in Riverton."}]}`},
		[]string{`{"links":[]}`},
	)
	ex := NewTwoPassLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{
			{ID: "D1:1", Role: "user", Text: "Avery lives in Riverton."},
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

func TestTwoPassLLMExtractor_DeterministicallyGroundsVerifiedSourceHint(t *testing.T) {
	client := fakeTwoPassLLM(
		[]string{`{"facts":[{"text":"Avery named the project Luma Lantern.","subject":"Avery","source_ids":["D1:1"],"quote":"It is called Luma Lantern."}]}`},
		[]string{`{"facts":[{"text":"Avery named the project Luma Lantern.","kind":"event","subject":"Avery","source_ids":["D1:1"],"quote":"It is called Luma Lantern."}]}`},
		[]string{`{"facts":[{"text":"Avery named the project Luma Lantern.","subject":"Avery","predicate":"named","object":"Luma Lantern","source_ids":["D1:1"],"quote":"It is called Luma Lantern."}]}`},
		[]string{`{"facts":[{"text":"Avery named the project Luma Lantern.","subject":"Avery","entities":["Avery","Luma Lantern"],"source_ids":["D1:1"],"quote":"It is called Luma Lantern."}]}`},
		nil,
	)
	client.Err = errors.New("content filter")
	ex := NewTwoPassLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{
			{ID: "D1:1", Role: "user", Speaker: "Avery", Text: "It is called Luma Lantern."},
			{ID: "D1:2", Role: "assistant", Speaker: "Rowan", Text: "That name is easy to remember."},
		},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want deterministic grounding fact, got %d: %+v", len(out), out)
	}
	if len(out[0].EvidenceRefs) != 1 || out[0].EvidenceRefs[0].ID != "D1:1" || out[0].EvidenceRefs[0].Text != "It is called Luma Lantern." {
		t.Fatalf("deterministic evidence refs = %+v, want D1:1 quote", out[0].EvidenceRefs)
	}
}

func TestTwoPassLLMExtractor_BackfillsHighSignalUncoveredTurns(t *testing.T) {
	client := fakeTwoPassLLM(
		[]string{
			`{"facts":[{"text":"Avery likes Riverton.","subject":"Avery","source_ids":["D1:1"],"quote":"Avery likes Riverton."}]}`,
			`{"facts":[{"text":"Avery bought 2 ceramic figurines yesterday for her family.","subject":"Avery","source_ids":["D1:2"],"quote":"I bought 2 ceramic figurines yesterday"}]}`,
		},
		[]string{
			`{"facts":[{"text":"Avery likes Riverton.","kind":"preference","subject":"Avery","source_ids":["D1:1"],"quote":"Avery likes Riverton."}]}`,
			`{"facts":[{"text":"Avery bought 2 ceramic figurines yesterday for her family.","kind":"event","subject":"Avery","source_ids":["D1:2"],"quote":"I bought 2 ceramic figurines yesterday"}]}`,
		},
		[]string{
			`{"facts":[{"text":"Avery likes Riverton.","subject":"Avery","predicate":"likes","object":"Riverton","source_ids":["D1:1"],"quote":"Avery likes Riverton."}]}`,
			`{"facts":[{"text":"Avery bought 2 ceramic figurines yesterday for her family.","subject":"Avery","predicate":"bought","object":"2 ceramic figurines","source_ids":["D1:2"],"quote":"I bought 2 ceramic figurines yesterday"}]}`,
		},
		[]string{
			`{"facts":[{"text":"Avery likes Riverton.","subject":"Avery","entities":["Avery","Riverton"],"source_ids":["D1:1"],"quote":"Avery likes Riverton."}]}`,
			`{"facts":[{"text":"Avery bought 2 ceramic figurines yesterday for her family.","subject":"Avery","entities":["Avery","2 ceramic figurines","family"],"source_ids":["D1:2"],"quote":"I bought 2 ceramic figurines yesterday"}]}`,
		},
		[]string{
			`{"links":[{"fact_index":0,"evidence_refs":[{"id":"D1:1","text":"Avery likes Riverton."}]}]}`,
			`{"links":[{"fact_index":0,"evidence_refs":[{"id":"D1:2","text":"I bought 2 ceramic figurines yesterday"}]}]}`,
		},
	)
	ex := NewTwoPassLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{
			{ID: "D1:1", Role: "user", Speaker: "Avery", Text: "Avery likes Riverton."},
			{ID: "D1:2", Role: "user", Speaker: "Avery", Text: "I bought 2 ceramic figurines yesterday for my family."},
			{ID: "D1:3", Role: "assistant", Speaker: "Rowan", Text: "Hey Avery, how are you doing?"},
		},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want original memory plus one coverage repair fact, got %d: %+v", len(out), out)
	}
	got := out[1]
	if got.Content != "Avery bought 2 ceramic figurines yesterday for her family." {
		t.Fatalf("repair content = %q", got.Content)
	}
	if got.Kind != domain.KindEvent {
		t.Fatalf("repair kind = %q, want event", got.Kind)
	}
	if len(got.EvidenceRefs) != 1 || got.EvidenceRefs[0].ID != "D1:2" {
		t.Fatalf("repair evidence refs = %+v, want D1:2", got.EvidenceRefs)
	}
	if len(client.Messages) != 10 {
		t.Fatalf("coverage repair should run four field extractors plus grounding twice, got %d calls", len(client.Messages))
	}
	var repairPrompt string
	for _, msgs := range client.Messages {
		if len(msgs) < 2 || msgs[0].Content() != TwoPassFactExtractionPrompt {
			continue
		}
		if strings.Contains(msgs[1].Content(), `"id":"D1:2"`) &&
			!strings.Contains(msgs[1].Content(), `"id":"D1:1"`) &&
			!strings.Contains(msgs[1].Content(), `"id":"D1:3"`) {
			repairPrompt = msgs[1].Content()
			break
		}
	}
	if !strings.Contains(repairPrompt, `"id":"D1:2"`) || strings.Contains(repairPrompt, `"id":"D1:3"`) {
		t.Fatalf("repair prompt should include only high-signal uncovered turns, got %q", repairPrompt)
	}
}

func TestTwoPassLLMExtractor_BackfillsWhenMemoryPassReturnsEmpty(t *testing.T) {
	client := fakeTwoPassLLM(
		[]string{
			`{"facts":[]}`,
			`{"facts":[{"text":"Avery visited the beach on 2023-05-07 with the kids.","subject":"Avery","source_ids":["D1:7"],"quote":"We visited the beach on 2023-05-07"}]}`,
		},
		[]string{
			`{"facts":[]}`,
			`{"facts":[{"text":"Avery visited the beach on 2023-05-07 with the kids.","kind":"event","subject":"Avery","source_ids":["D1:7"],"quote":"We visited the beach on 2023-05-07"}]}`,
		},
		[]string{
			`{"facts":[]}`,
			`{"facts":[{"text":"Avery visited the beach on 2023-05-07 with the kids.","subject":"Avery","predicate":"visited","object":"beach","source_ids":["D1:7"],"quote":"We visited the beach on 2023-05-07"}]}`,
		},
		[]string{
			`{"facts":[]}`,
			`{"facts":[{"text":"Avery visited the beach on 2023-05-07 with the kids.","subject":"Avery","entities":["Avery","beach","kids"],"source_ids":["D1:7"],"quote":"We visited the beach on 2023-05-07"}]}`,
		},
		[]string{`{"links":[{"fact_index":0,"evidence_refs":[{"id":"D1:7","text":"We visited the beach on 2023-05-07"}]}]}`},
	)
	ex := NewTwoPassLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID: "D1:7", Role: "user", Speaker: "Avery", Text: "We visited the beach on 2023-05-07 and the kids had a blast.",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(client.Messages) != 9 {
		t.Fatalf("empty initial pass should run initial field extractors plus targeted field extractors and grounding, got %d calls", len(client.Messages))
	}
	if len(out) != 1 {
		t.Fatalf("want one repair fact, got %d: %+v", len(out), out)
	}
	if len(out[0].EvidenceRefs) != 1 || out[0].EvidenceRefs[0].ID != "D1:7" {
		t.Fatalf("repair evidence refs = %+v, want D1:7", out[0].EvidenceRefs)
	}
}

func TestTwoPassLLMExtractor_CoverageRepairUsesMultilingualTextSignals(t *testing.T) {
	client := fakeTwoPassLLM(
		[]string{
			`{"facts":[]}`,
			`{"facts":[{"text":"Avery bought 3 books yesterday, including 小王子.","subject":"Avery","source_ids":["D1:8"],"quote":"bought 3 books yesterday"}]}`,
		},
		[]string{
			`{"facts":[]}`,
			`{"facts":[{"text":"Avery bought 3 books yesterday, including 小王子.","kind":"event","subject":"Avery","source_ids":["D1:8"],"quote":"bought 3 books yesterday"}]}`,
		},
		[]string{
			`{"facts":[]}`,
			`{"facts":[]}`,
		},
		[]string{
			`{"facts":[]}`,
			`{"facts":[{"text":"Avery bought 3 books yesterday, including 小王子.","subject":"Avery","entities":["Avery","小王子"],"source_ids":["D1:8"],"quote":"bought 3 books yesterday"}]}`,
		},
		[]string{`{"links":[{"fact_index":0,"evidence_refs":[{"id":"D1:8","text":"bought 3 books yesterday"}]}]}`},
	)
	ex := NewTwoPassLLMExtractor(client)
	out, err := ex.Extract(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{
			ID: "D1:8", Role: "user", Speaker: "Avery", Text: "I bought 3 books yesterday, including 「小王子」。",
		}},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(client.Messages) != 9 {
		t.Fatalf("Chinese time/quote signals should trigger targeted repair, got %d calls", len(client.Messages))
	}
	if len(out) != 1 || len(out[0].EvidenceRefs) != 1 || out[0].EvidenceRefs[0].ID != "D1:8" {
		t.Fatalf("repair output = %+v, want one fact grounded to D1:8", out)
	}
}

func TestCoverageRepairInput_DoesNotBudgetHighSignalTurns(t *testing.T) {
	input := port.IngestInput{Turns: make([]port.TurnContext, 0, 10)}
	for i := 0; i < 10; i++ {
		input.Turns = append(input.Turns, port.TurnContext{
			ID:         "turn-id-" + string(rune('a'+i)),
			EvidenceID: "D1:" + string(rune('0'+i)),
			Text:       "Avery bought 2 ceramic figurines yesterday for her family.",
		})
	}
	repairInput, ok := buildCoverageRepairInput(input, nil)
	if !ok {
		t.Fatal("expected high-signal repair input")
	}
	if len(repairInput.Turns) != len(input.Turns) {
		t.Fatalf("repair turns = %d, want all %d high-signal turns", len(repairInput.Turns), len(input.Turns))
	}
}

func TestCoverageRepairFacts_AreGroundedTaggedAndDedupeLocally(t *testing.T) {
	base := []domain.TemporalFact{{
		Content:      "Avery likes Riverton.",
		EvidenceRefs: []domain.EvidenceRef{{ID: "D1:1", Text: "Avery likes Riverton."}},
	}}
	repaired := []domain.TemporalFact{
		{
			Content:      "Avery bought 2 ceramic figurines yesterday for her family.",
			EvidenceRefs: []domain.EvidenceRef{{ID: "D1:2", Text: "I bought 2 ceramic figurines yesterday."}},
		},
		{
			Content:      "Avery bought 2 ceramic figurines yesterday for the family.",
			EvidenceRefs: []domain.EvidenceRef{{ID: "D1:2", Text: "I bought 2 ceramic figurines yesterday."}},
		},
		{
			Content: "ungrounded repair noise",
		},
	}
	out := appendCoverageRepairFacts(base, repaired)
	if len(out) != 2 {
		t.Fatalf("expected base plus one deduped repair fact, got %d: %+v", len(out), out)
	}
	if got, _ := out[1].Metadata[domain.MetaCoverageRepair].(bool); !got {
		t.Fatalf("repair fact should be tagged, metadata=%v", out[1].Metadata)
	}
	if len(out[1].EvidenceRefs) != 1 || out[1].EvidenceRefs[0].ID != "D1:2" {
		t.Fatalf("repair evidence should be preserved and deduped, got %+v", out[1].EvidenceRefs)
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
		"signed up for a ceramics class",
		`NOT {kind:"state"`,
		"Single-occurrence dated\n                     actions are events, not states",
		"Be exhaustive about concrete, retrievable details",
		"Split enumerations into separate memories",
		"Mira enjoys salsa dancing",
		"Do not\n  collapse lists into",
		"Preserve literal answer-bearing spans",
		"that book",
		"Be careful with second-person comments",
		"second-person detail is about the addressee",
		"subject -> predicate -> object",
		"MUST be filled as a pair",
		"Never emit an object without a predicate",
		"Do not invent an object",
		"planning to repair a bicycle",
		`"procedure"`,
		"When comparing options, use a markdown\n                     table.",
		"Quote proper nouns verbatim",
		"The Glass Compass",
		"<source_turns>",
		"Ground each memory in the DIRECT source turn",
		"Do not cite neighbouring turns",
		"Do not create cross-turn summary memories",
		"Use multiple evidence_refs only when one memory\n  truly requires both turns together",
		"Prefer quoting the exact\n  words that make the memory true",
	}
	for _, s := range mustContain {
		if !strings.Contains(LLMExtractorSystemPrompt, s) {
			t.Errorf("LLMExtractorSystemPrompt missing anti-abstraction guard: %q", s)
		}
	}
}

func TestTwoPassPrompts_GuardCoverageAndGrounding(t *testing.T) {
	contentMustContain := []string{
		"XML-tagged envelope",
		"<source_turns>",
		"objective facts",
		"directly supported by source turns",
		`Output only {"facts"`,
		"Be exhaustive about concrete, retrievable details",
		"Preserve literal answer-bearing spans",
		"that book",
		"Be careful with second-person comments",
		"second-person detail is about the addressee",
		"Never output pronouns as \"subject\"",
		"signed up for a ceramics class yesterday",
		"NOT \"Mira uses ceramics for self-expression.\"",
		"Do not create cross-turn summary facts",
	}
	for _, s := range contentMustContain {
		if !strings.Contains(TwoPassFactExtractionPrompt, s) {
			t.Errorf("TwoPassFactExtractionPrompt missing coverage guard: %q", s)
		}
	}
	kindMustContain := []string{
		"objective facts",
		"directly supported by source",
		"event, state, preference, procedure, relation, plan, note",
		"Do NOT turn a one-off dated action into state",
		"Never use pronouns as \"subject\"",
	}
	for _, s := range kindMustContain {
		if !strings.Contains(TwoPassKindExtractionPrompt, s) {
			t.Errorf("TwoPassKindExtractionPrompt missing guard: %q", s)
		}
	}
	relationMustContain := []string{
		"objective claim",
		"Fill predicate/object as a pair",
		"Never use pronouns",
		"Be conservative",
		"Do not invent an object",
	}
	for _, s := range relationMustContain {
		if !strings.Contains(TwoPassRelationExtractionPrompt, s) {
			t.Errorf("TwoPassRelationExtractionPrompt missing guard: %q", s)
		}
	}
	entityMustContain := []string{
		"objective claim",
		"subject\" must follow the same stable-anchor rule",
		"planning to repair a bicycle",
		"shortest stable noun phrase",
	}
	for _, s := range entityMustContain {
		if !strings.Contains(TwoPassEntityExtractionPrompt, s) {
			t.Errorf("TwoPassEntityExtractionPrompt missing guard: %q", s)
		}
	}

	groundingMustContain := []string{
		"XML-tagged envelope with two sections",
		`<source_turns format="jsonl">`,
		`<facts format="json">`,
		"candidate facts",
		"The facts list is not evidence",
		`"text":"<verbatim quote>"`,
		"direct source turn",
		"Prefer the turn with exact entity/date/item\n  surface forms",
		"cite the\n  answer turn for answer details",
		"Do not cite neighbouring acknowledgements",
		"Use ids exactly as they appear",
		"Prefer one direct evidence id for one atomic fact",
		"evidence_refs[].text",
		"exact words that make the fact true",
	}
	for _, s := range groundingMustContain {
		if !strings.Contains(TwoPassEvidenceGroundingPrompt, s) {
			t.Errorf("TwoPassEvidenceGroundingPrompt missing grounding guard: %q", s)
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
