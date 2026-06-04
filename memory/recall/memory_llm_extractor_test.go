package recall

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
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
		body = `{"proposals":[]}`
	}
	return llm.NewTextMessage(llm.RoleAssistant, body), llm.TokenUsage{}, nil
}

func (s *scriptedLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("scriptedLLM: streaming not implemented")
}

func TestWithLLMExtractor_PromotesParametersAndArbitratesStructuredSemanticDuplicate(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	observations := NewInMemoryObservationStore()
	links := NewInMemoryLinkStore()
	client := &scriptedLLM{ResponsesBySchemaName: map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"D1:1","families":["parameter_slot","semantic_fact"]}]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[
			{"family":"parameter_slot","source_ids":["D1:1"],"quote":"temperature = 0.2","owner":"","name_surface":"temperature","operation_surface":"","value_surface":"0.2","normalized_value_hint":"0.2","old_value_surface":"","condition_surface":"","operator_surface":"","effective_time_surface":""},
			{"family":"parameter_slot","source_ids":["D1:1"],"quote":"top_p = 0.9","owner":"experiment","name_surface":"top_p","operation_surface":"","value_surface":"0.9","normalized_value_hint":"0.9","old_value_surface":"","condition_surface":"","operator_surface":"","effective_time_surface":""},
			{"family":"parameter_slot","source_ids":["D1:1"],"quote":"max tokens = 4096","owner":"experiment","name_surface":"max tokens","operation_surface":"","value_surface":"4096","normalized_value_hint":"4096","old_value_surface":"","condition_surface":"","operator_surface":"","effective_time_surface":""}
		]}`,
		"recall_semantic_proposals_semantic_fact": `{"proposals":[{
			"family":"semantic_fact",
			"text":"temperature is 0.2",
			"kind":"state",
			"subject":"",
			"predicate":"temperature",
			"object":"0.2",
			"entities":["experiment","temperature"],
			"source_ids":["D1:1"],
			"quote":"temperature = 0.2"
		}]}`,
	}}
	mem, err := New(
		WithTemporalStore(store),
		WithObservationStore(observations),
		WithLinkStore(links),
		WithLLMExtractor(client),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{
			ID:   "D1:1",
			Role: "user",
			Text: "这次实验 temperature = 0.2, top_p = 0.9, max tokens = 4096.",
		}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 3 {
		t.Fatalf("fact ids = %v, want only 3 parameter facts", res.FactIDs)
	}
	gotNames := map[string]bool{}
	for _, id := range res.FactIDs {
		fact, err := store.Get(ctx, scope, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if fact.Kind != FactParameter {
			t.Fatalf("fact %s kind = %q, want parameter: %+v", id, fact.Kind, fact)
		}
		gotNames[fact.Metadata[MetaParameterCanonicalName].(string)] = true
		if fact.Metadata[MetaParameterValueKind] != "number" {
			t.Fatalf("parameter value kind = %v, want number", fact.Metadata[MetaParameterValueKind])
		}
		if len(fact.EvidenceRefs) == 0 || fact.EvidenceRefs[0].ObservationID == "" || fact.EvidenceRefs[0].SpanID == "" {
			t.Fatalf("parameter evidence not canonical: %+v", fact.EvidenceRefs)
		}
		gotLinks, err := links.List(ctx, scope, port.LinkListQuery{})
		if err != nil {
			t.Fatalf("links.List: %v", err)
		}
		if !hasLink(gotLinks, LinkDerivedFrom, GraphNodeAssertion, id, GraphNodeObservationSpan, fact.EvidenceRefs[0].SpanID) {
			t.Fatalf("missing derived_from span link for %s in %+v", id, gotLinks)
		}
	}
	for _, name := range []string{"temperature", "top_p", "max_tokens"} {
		if !gotNames[name] {
			t.Fatalf("missing canonical parameter %q in %v", name, gotNames)
		}
	}
}

func TestWithLLMExtractor_DropsInferredSemanticProposal(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{ResponsesBySchemaName: map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"D1:infer","families":["semantic_fact"]}]}`,
		"recall_semantic_proposals_semantic_fact": `{"proposals":[{
			"family":"semantic_fact",
			"text":"Alice probably prefers Paris.",
			"kind":"preference",
			"subject":"Alice",
			"predicate":"prefers",
			"object":"Paris",
			"entities":["Alice","Paris"],
			"source_ids":["D1:infer"],
			"quote":"Alice mentioned Paris."
		}]}`,
	}}
	mem, err := New(WithTemporalStore(store), WithLLMExtractor(client))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "D1:infer", Role: "user", Text: "Alice mentioned Paris."}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 0 {
		t.Fatalf("inferred semantic proposal promoted facts: %v", res.FactIDs)
	}
}

func TestWithLLMExtractor_SemanticFactUsesGroundedProposalTextNotLLMCanonicalFields(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{ResponsesBySchemaName: map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"D1:semantic","families":["semantic_fact"]}]}`,
		"recall_semantic_proposals_semantic_fact": `{"proposals":[{
			"family":"semantic_fact",
			"text":"Alice likes tea.",
			"kind":"relation",
			"subject":"Mallory",
			"predicate":"invented",
			"object":"false object",
			"entities":["Mallory"],
			"source_ids":["D1:semantic"],
			"quote":"Alice likes tea."
		}]}`,
	}}
	mem, err := New(WithTemporalStore(store), WithLLMExtractor(client))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "D1:semantic", Role: "user", Text: "Alice likes tea."}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 1 {
		t.Fatalf("fact ids = %v, want one grounded semantic fact", res.FactIDs)
	}
	fact, err := store.Get(ctx, scope, res.FactIDs[0])
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if fact.Content != "Alice likes tea." {
		t.Fatalf("content = %q, want grounded proposal text", fact.Content)
	}
	if fact.Predicate == "invented" || fact.Object == "false object" {
		t.Fatalf("fact trusted LLM canonical fields: %+v", fact)
	}
}

func TestWithLLMExtractor_RejectsParameterValueNotGroundedInSource(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{ResponsesBySchemaName: map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"D1:missing-value","families":["parameter_slot"]}]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["D1:missing-value"],
			"quote":"temperature",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"",
			"value_surface":"0.2",
			"normalized_value_hint":"0.2",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"",
			"effective_time_surface":"",
			"confirmation_source_ids":["ack"],
			"confirmation_quote":"是的."
		}]}`,
	}}
	mem, err := New(WithTemporalStore(store), WithLLMExtractor(client))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "D1:missing-value", Role: "user", Text: "temperature"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 0 {
		t.Fatalf("ungrounded parameter value promoted facts: %v", res.FactIDs)
	}
}

func TestWithLLMExtractor_RejectsAmbiguousParallelParameterPairing(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{ResponsesBySchemaName: map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"D1:parallel","families":["parameter_slot"]}]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["D1:parallel"],
			"quote":"temperature, top_p = 0.2, 0.9",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"",
			"value_surface":"0.9",
			"normalized_value_hint":"0.9",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"",
			"effective_time_surface":"",
			"confirmation_source_ids":["ack"],
			"confirmation_quote":"是的."
		}]}`,
	}}
	mem, err := New(WithTemporalStore(store), WithLLMExtractor(client))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "D1:parallel", Role: "user", Text: "temperature, top_p = 0.2, 0.9"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 0 {
		t.Fatalf("ambiguous parallel pairing promoted facts: %v", res.FactIDs)
	}
}

func TestWithLLMExtractor_RejectsParameterProximityWithoutPairingSyntax(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{ResponsesBySchemaName: map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"D1:near","families":["parameter_slot"]}]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["D1:near"],
			"quote":"temperature near top_p 0.9",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"",
			"value_surface":"0.9",
			"normalized_value_hint":"0.9",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"",
			"effective_time_surface":""
		}]}`,
	}}
	mem, err := New(WithTemporalStore(store), WithLLMExtractor(client))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "D1:near", Role: "user", Text: "temperature near top_p 0.9"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 0 {
		t.Fatalf("proximity-only pairing promoted facts: %v", res.FactIDs)
	}
}

func TestWithLLMExtractor_RunsTypedExtractionPerSegment(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	client := &scriptedLLM{ResponsesBySchemaName: map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[
			{"segment_id":"D1","families":["parameter_slot"]},
			{"segment_id":"D2","families":["parameter_slot"]}
		]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[]}`,
	}}
	mem, err := New(WithTemporalStore(temporalstore.NewMemoryStore()), WithLLMExtractor(client))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{
			{ID: "D1", Role: "user", Text: "temperature 0.2"},
			{ID: "D2", Role: "user", Text: "top_p 0.9"},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	var parameterCalls int
	for _, opts := range client.Options {
		got := llm.GenerateOptions{}
		for _, opt := range opts {
			opt(&got)
		}
		if got.JSONSchema != nil && got.JSONSchema.Name == "recall_semantic_proposals_parameter_slot" {
			parameterCalls++
		}
	}
	if parameterCalls < 2 {
		t.Fatalf("parameter extractor calls = %d, want bounded calls per routed canonical segment", parameterCalls)
	}
}

func TestWithLLMExtractor_RecentContextCannotGroundParameter(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{ResponsesBySchemaName: map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"ack","families":["parameter_slot"]}]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["ack"],
			"quote":"是的.",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"",
			"value_surface":"0.2",
			"normalized_value_hint":"0.2",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"",
			"effective_time_surface":"",
			"confirmation_source_ids":["ack"],
			"confirmation_quote":"是的."
		}]}`,
	}}
	mem, err := New(WithTemporalStore(store), WithLLMExtractor(client))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Turns:          []TurnContext{{ID: "ack", Role: "user", Text: "是的."}},
		RecentMessages: []Message{{Role: "user", Text: "确认 temperature 调成 0.2 吗?"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 0 {
		t.Fatalf("recent context grounded facts: %v", res.FactIDs)
	}
}

func TestWithLLMExtractor_EvidenceWindowEnablesDialogueConfirmedParameter(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	observations := NewInMemoryObservationStore()
	links := NewInMemoryLinkStore()
	client := &scriptedLLM{}
	mem, err := New(
		WithTemporalStore(store),
		WithObservationStore(observations),
		WithLinkStore(links),
		WithLLMExtractor(client),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "prior", Role: "user", Text: "temperature = 0.2?"}},
	}); err != nil {
		t.Fatalf("save prior observation: %v", err)
	}
	raw, err := observations.List(ctx, scope, port.ObservationListQuery{SourceID: "prior"})
	if err != nil {
		t.Fatalf("observations.List: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("prior observations = %+v", raw)
	}
	client.ResponsesBySchemaName = map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"prior","families":["parameter_slot"]}]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["prior"],
			"quote":"temperature = 0.2?",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"",
			"value_surface":"0.2",
			"normalized_value_hint":"0.2",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"=",
			"effective_time_surface":"",
			"confirmation_source_ids":["ack"],
			"confirmation_quote":"是的."
		}]}`,
	}
	res, trace, err := mem.(SaveDebugExplainer).SaveExplainDebug(ctx, scope, SaveRequest{
		Turns:              []TurnContext{{ID: "ack", Role: "user", Text: "是的."}},
		EvidenceWindowRefs: []EvidenceWindowRef{{ObservationID: raw[0].ID}},
	})
	if err != nil {
		t.Fatalf("save confirmation: %v", err)
	}
	if len(res.FactIDs) != 1 {
		t.Fatalf("fact ids = %v, trace=%+v, want dialogue-confirmed parameter", res.FactIDs, trace)
	}
	fact, err := store.Get(ctx, scope, res.FactIDs[0])
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if fact.Kind != FactParameter {
		t.Fatalf("kind = %q, want parameter", fact.Kind)
	}
	if fact.Metadata[MetaParameterGroundingLevel] != "dialogue_confirmed" {
		t.Fatalf("grounding = %v, want dialogue_confirmed", fact.Metadata[MetaParameterGroundingLevel])
	}
	if len(fact.EvidenceRefs) != 2 {
		t.Fatalf("evidence refs = %+v, want prior + confirmation", fact.EvidenceRefs)
	}
}

func TestWithLLMExtractor_RejectsAmbiguousDialogueConfirmationWindow(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	observations := NewInMemoryObservationStore()
	client := &scriptedLLM{}
	mem, err := New(
		WithTemporalStore(store),
		WithObservationStore(observations),
		WithLLMExtractor(client),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "prior-temp", Role: "user", SessionID: "s1", Text: "Confirm temperature = 0.2?"}},
	}); err != nil {
		t.Fatalf("save temp prior: %v", err)
	}
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "prior-top-p", Role: "user", SessionID: "s1", Text: "Confirm top_p = 0.9?"}},
	}); err != nil {
		t.Fatalf("save top_p prior: %v", err)
	}
	tempObs, err := observations.List(ctx, scope, port.ObservationListQuery{SourceID: "prior-temp"})
	if err != nil {
		t.Fatalf("temp observations: %v", err)
	}
	topPObs, err := observations.List(ctx, scope, port.ObservationListQuery{SourceID: "prior-top-p"})
	if err != nil {
		t.Fatalf("top_p observations: %v", err)
	}
	if len(tempObs) != 1 || len(topPObs) != 1 {
		t.Fatalf("prior observations temp=%+v top_p=%+v", tempObs, topPObs)
	}
	client.ResponsesBySchemaName = map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"prior-temp","families":["parameter_slot"]}]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["prior-temp"],
			"quote":"Confirm temperature = 0.2?",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"Confirm",
			"value_surface":"0.2",
			"normalized_value_hint":"0.2",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"",
			"effective_time_surface":""
		}]}`,
	}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "ack-ambiguous", Role: "user", SessionID: "s1", Text: "是的."}},
		EvidenceWindowRefs: []EvidenceWindowRef{
			{ObservationID: tempObs[0].ID},
			{ObservationID: topPObs[0].ID},
		},
	})
	if err != nil {
		t.Fatalf("save ambiguous confirmation: %v", err)
	}
	if len(res.FactIDs) != 0 {
		t.Fatalf("ambiguous confirmation promoted facts: %v", res.FactIDs)
	}
}

func TestWithLLMExtractor_RejectsNegatedDialogueConfirmation(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	observations := NewInMemoryObservationStore()
	client := &scriptedLLM{}
	mem, err := New(WithTemporalStore(store), WithObservationStore(observations), WithLLMExtractor(client))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "prior-negated", Role: "user", SessionID: "s1", Text: "Confirm temperature = 0.2?"}},
	}); err != nil {
		t.Fatalf("save prior observation: %v", err)
	}
	prior, err := observations.List(ctx, scope, port.ObservationListQuery{SourceID: "prior-negated"})
	if err != nil {
		t.Fatalf("prior observations: %v", err)
	}
	if len(prior) != 1 {
		t.Fatalf("prior observations = %+v", prior)
	}
	client.ResponsesBySchemaName = map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"prior-negated","families":["parameter_slot"]}]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["prior-negated"],
			"quote":"Confirm temperature = 0.2?",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"Confirm",
			"value_surface":"0.2",
			"normalized_value_hint":"0.2",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"",
			"effective_time_surface":""
		}]}`,
	}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Turns:              []TurnContext{{ID: "ack-negated", Role: "user", SessionID: "s1", Text: "not yes."}},
		EvidenceWindowRefs: []EvidenceWindowRef{{ObservationID: prior[0].ID}},
	})
	if err != nil {
		t.Fatalf("save negated confirmation: %v", err)
	}
	if len(res.FactIDs) != 0 {
		t.Fatalf("negated confirmation promoted facts: %v", res.FactIDs)
	}
}

func TestWithLLMExtractor_ConditionalParameterContentKeepsCondition(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{ResponsesBySchemaName: map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"D1:condition","families":["parameter_slot"]}]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["D1:condition"],
			"quote":"temperature = 0.2 when GPU is enabled",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"",
			"value_surface":"0.2",
			"normalized_value_hint":"0.2",
			"old_value_surface":"",
			"condition_surface":"GPU is enabled",
			"operator_surface":"",
			"effective_time_surface":""
		}]}`,
	}}
	mem, err := New(WithTemporalStore(store), WithLLMExtractor(client))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "D1:condition", Role: "user", Text: "temperature = 0.2 when GPU is enabled"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 1 {
		t.Fatalf("fact ids = %v, want one conditional parameter", res.FactIDs)
	}
	fact, err := store.Get(ctx, scope, res.FactIDs[0])
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if !strings.Contains(fact.Content, "when GPU is enabled") {
		t.Fatalf("conditional content lost condition: %q", fact.Content)
	}
}

func TestWithLLMExtractor_AsyncSemanticMatchesSyncParameterPromotion(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	responses := map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"D1:async","families":["parameter_slot"]}]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["D1:async"],
			"quote":"temperature = 0.2",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"",
			"value_surface":"0.2",
			"normalized_value_hint":"0.2",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"",
			"effective_time_surface":""
		}]}`,
	}
	syncStore := temporalstore.NewMemoryStore()
	syncMem, err := New(
		WithTemporalStore(syncStore),
		WithLLMExtractor(&scriptedLLM{ResponsesBySchemaName: responses}),
	)
	if err != nil {
		t.Fatalf("sync new: %v", err)
	}
	if _, err := syncMem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "D1:async", Role: "user", Text: "temperature = 0.2"}},
	}); err != nil {
		t.Fatalf("sync save: %v", err)
	}
	syncFacts, err := syncStore.List(ctx, scope, port.ListQuery{Kinds: []FactKind{FactParameter}})
	if err != nil {
		t.Fatalf("sync list: %v", err)
	}
	queue := NewInMemoryAsyncSemanticQueue()
	asyncStore := temporalstore.NewMemoryStore()
	asyncObservations := NewInMemoryObservationStore()
	asyncLinks := NewInMemoryLinkStore()
	asyncMem, err := New(
		WithTemporalStore(asyncStore),
		WithObservationStore(asyncObservations),
		WithLinkStore(asyncLinks),
		WithAsyncSemanticQueue(queue),
		WithLLMExtractor(&scriptedLLM{ResponsesBySchemaName: responses}),
	)
	if err != nil {
		t.Fatalf("async new: %v", err)
	}
	if _, err := asyncMem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "D1:async", Role: "user", Text: "temperature = 0.2"}},
	}); err != nil {
		t.Fatalf("async save: %v", err)
	}
	rawBefore, err := asyncObservations.List(ctx, scope, port.ObservationListQuery{SourceID: "D1:async"})
	if err != nil {
		t.Fatalf("async observations before process: %v", err)
	}
	if len(rawBefore) != 1 {
		t.Fatalf("async raw observations before process = %+v", rawBefore)
	}
	proc, ok := NewAsyncSemanticProcessor(asyncMem)
	if !ok {
		t.Fatal("missing async semantic processor")
	}
	if _, err := proc.ProcessAsyncSemantic(ctx, AsyncSemanticProcessOptions{Limit: 1, Scope: scope}); err != nil {
		t.Fatalf("process async semantic: %v", err)
	}
	asyncFacts, err := asyncStore.List(ctx, scope, port.ListQuery{Kinds: []FactKind{FactParameter}})
	if err != nil {
		t.Fatalf("async list: %v", err)
	}
	if len(syncFacts) != 1 || len(asyncFacts) != 1 {
		t.Fatalf("sync facts=%+v async facts=%+v", syncFacts, asyncFacts)
	}
	for _, fact := range []TemporalFact{syncFacts[0], asyncFacts[0]} {
		if fact.Metadata[MetaParameterCanonicalName] != "temperature" ||
			fact.Metadata[MetaParameterNormalizedValue] != "0.2" ||
			fact.Metadata[MetaParameterGroundingLevel] != "exact" {
			t.Fatalf("unexpected parameter metadata: %+v", fact.Metadata)
		}
		if len(fact.EvidenceRefs) != 1 || fact.EvidenceRefs[0].Text != "temperature = 0.2" || fact.EvidenceRefs[0].SpanID == "" {
			t.Fatalf("unexpected evidence refs: %+v", fact.EvidenceRefs)
		}
	}
	if asyncFacts[0].EvidenceRefs[0].ObservationID != rawBefore[0].ID {
		t.Fatalf("async worker used observation %q, want original async Save observation %q",
			asyncFacts[0].EvidenceRefs[0].ObservationID, rawBefore[0].ID)
	}
	rawAfter, err := asyncObservations.List(ctx, scope, port.ObservationListQuery{SourceID: "D1:async"})
	if err != nil {
		t.Fatalf("async observations after process: %v", err)
	}
	if len(rawAfter) != 1 {
		t.Fatalf("async worker created duplicate observations: %+v", rawAfter)
	}
	gotLinks, err := asyncLinks.List(ctx, scope, port.LinkListQuery{})
	if err != nil {
		t.Fatalf("async links list: %v", err)
	}
	if !hasLink(gotLinks, LinkDerivedFrom, GraphNodeAssertion, asyncFacts[0].ID, GraphNodeObservationSpan, asyncFacts[0].EvidenceRefs[0].SpanID) {
		t.Fatalf("async parameter missing span link: %+v", gotLinks)
	}
}

func TestWithLLMExtractor_AsyncSemanticEvidenceWindowDialogueConfirmed(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	observations := NewInMemoryObservationStore()
	links := NewInMemoryLinkStore()
	queue := NewInMemoryAsyncSemanticQueue()
	client := &scriptedLLM{}
	mem, err := New(
		WithTemporalStore(store),
		WithObservationStore(observations),
		WithLinkStore(links),
		WithAsyncSemanticQueue(queue),
		WithLLMExtractor(client),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{ID: "prior-async", Role: "user", Text: "temperature = 0.2?"}},
	}); err != nil {
		t.Fatalf("save prior observation: %v", err)
	}
	rawPrior, err := observations.List(ctx, scope, port.ObservationListQuery{SourceID: "prior-async"})
	if err != nil {
		t.Fatalf("prior observations: %v", err)
	}
	if len(rawPrior) != 1 {
		t.Fatalf("prior observations = %+v", rawPrior)
	}
	client.ResponsesBySchemaName = map[string]string{
		"recall_semantic_proposals_segment_classifier": `{"segments":[{"segment_id":"prior-async","families":["parameter_slot"]}]}`,
		"recall_semantic_proposals_parameter_slot": `{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["prior-async"],
			"quote":"temperature = 0.2?",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"",
			"value_surface":"0.2",
			"normalized_value_hint":"0.2",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"=",
			"effective_time_surface":"",
			"confirmation_source_ids":["ack-async"],
			"confirmation_quote":"是的."
		}]}`,
	}
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Mode:               WriteModeAsyncSemantic,
		Turns:              []TurnContext{{ID: "ack-async", Role: "user", Text: "是的."}},
		EvidenceWindowRefs: []EvidenceWindowRef{{ObservationID: rawPrior[0].ID}},
	}); err != nil {
		t.Fatalf("async save confirmation: %v", err)
	}
	proc, ok := NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("missing async semantic processor")
	}
	if _, err := proc.ProcessAsyncSemantic(ctx, AsyncSemanticProcessOptions{Limit: 1, Scope: scope}); err != nil {
		t.Fatalf("process async semantic: %v", err)
	}
	facts, err := store.List(ctx, scope, port.ListQuery{Kinds: []FactKind{FactParameter}})
	if err != nil {
		t.Fatalf("list parameter facts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("parameter facts = %+v, want one", facts)
	}
	if facts[0].Metadata[MetaParameterGroundingLevel] != "dialogue_confirmed" {
		t.Fatalf("grounding = %v, want dialogue_confirmed", facts[0].Metadata[MetaParameterGroundingLevel])
	}
	if len(facts[0].EvidenceRefs) != 2 {
		t.Fatalf("evidence refs = %+v, want prior + confirmation", facts[0].EvidenceRefs)
	}
	if facts[0].EvidenceRefs[0].ObservationID != rawPrior[0].ID && facts[0].EvidenceRefs[1].ObservationID != rawPrior[0].ID {
		t.Fatalf("prior evidence window not preserved in refs: %+v", facts[0].EvidenceRefs)
	}
}

func TestWithLLMExtractor_WiresExtractorIntoSavePath(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{Responses: []string{
		`{"segments":[{"segment_id":"D1:3","families":["semantic_fact"]}]}`,
		`{"proposals":[{
		"text":"Alice said Paris is her city.",
		"kind":"state",
		"subject":"Alice",
		"predicate":"",
		"object":"",
		"entities":["Alice","Paris"],
		"source_ids":["D1:3"],
		"quote":"Alice said Paris is her city."
	}]}`,
	}}

	mem, err := New(
		WithTemporalStore(store),
		WithLLMExtractor(
			client,
			WithLLMExtractorTemperature(0.2),
			WithLLMExtractorSchemaName("recall_semantic_proposals_v1"),
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
	if got.JSONSchema == nil || got.JSONSchema.Name != "recall_semantic_proposals_v1_semantic_fact" {
		t.Errorf("schema name option not propagated, got=%+v", got.JSONSchema)
	}
	if got.JSONMode == nil || !*got.JSONMode {
		t.Errorf("JSON mode should be enabled")
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
