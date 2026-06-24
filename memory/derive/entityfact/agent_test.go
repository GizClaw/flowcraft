package entityfact

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory/derive"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	sdkllm "github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestAgentExtractorMaterializesStableEntitiesAndFacts(t *testing.T) {
	ctx := context.Background()
	fake := &agentFakeLLM{replies: []string{`{
		"entities":[{"name":"Ada","type":"person","aliases":["Ada L."],"source_ids":["dia-1"],"confidence":0.9},{"name":"tea","type":"object","source_ids":["dia-1"],"confidence":0.9}],
		"facts":[]
	}`, `{
		"entities":[],
		"facts":[{"subject":"Ada","relation_type":"preference","predicate_text":"likes","object_names":["tea"],"fact_text":"Ada likes tea.","source_ids":["dia-1"],"confidence":0.95}]
	}`}}
	input := agentEntityFactInput([]sourcemessage.Message{
		{ID: "dia-1", ConversationID: "conv", Seq: 1, Message: model.NewTextMessage(model.RoleUser, "Ada likes tea.")},
	})

	out, err := (AgentExtractor{LLM: fake}).ExtractEntityFacts(ctx, input)
	if err != nil {
		t.Fatalf("ExtractEntityFacts error = %v", err)
	}
	if len(out.Entities) != 2 {
		t.Fatalf("entities len = %d, want subject and object entities: %+v", len(out.Entities), out.Entities)
	}
	if len(out.Facts) != 1 {
		t.Fatalf("facts len = %d, want one fact: %+v", len(out.Facts), out.Facts)
	}
	fact := out.Facts[0]
	if fact.ID == "" || fact.SubjectEntityID == "" || len(fact.ObjectEntityIDs) != 1 {
		t.Fatalf("fact = %+v, want stable linked fact", fact)
	}
	if fact.RelationType != viewentityfact.RelationPreference || fact.FactText != "Ada likes tea." {
		t.Fatalf("fact = %+v, want preference fact text", fact)
	}
	if got := fact.Metadata[viewentityfact.FactGraphableMetadataKey]; got != true || !viewentityfact.IsGraphableFact(fact) {
		t.Fatalf("fact graphable metadata = %v, want true", got)
	}
	if len(fake.calls) != 2 || fake.calls[0].jsonMode == nil || !*fake.calls[0].jsonMode || fake.calls[1].jsonMode == nil || !*fake.calls[1].jsonMode {
		t.Fatalf("fake calls = %+v, want entity and fact JSON-mode calls", fake.calls)
	}
	factControl := fake.calls[1].messages[1].Content()
	if !strings.Contains(factControl, "entity_catalog") || !strings.Contains(factControl, "Ada") || !strings.Contains(factControl, "tea") {
		t.Fatalf("fact control = %s, want entity catalog with Ada and tea", factControl)
	}
}

func TestAgentExtractorDropsFactWithMissingSubjectOrObjectEntity(t *testing.T) {
	ctx := context.Background()
	fake := &agentFakeLLM{replies: []string{`{
		"entities":[{"name":"Ada","type":"person","source_ids":["dia-1"],"confidence":0.9}],
		"facts":[]
	}`, `{
		"entities":[],
		"facts":[
			{"subject":"Grace","relation_type":"preference","predicate_text":"likes","fact_text":"Grace likes tea.","source_ids":["dia-1"],"confidence":0.95},
			{"subject":"Ada","relation_type":"preference","predicate_text":"likes","object_names":["tea"],"fact_text":"Ada likes tea.","source_ids":["dia-1"],"confidence":0.95}
		]
	}`}}
	input := agentEntityFactInput([]sourcemessage.Message{
		{ID: "dia-1", ConversationID: "conv", Seq: 1, Message: model.NewTextMessage(model.RoleUser, "Ada likes tea.")},
	})

	out, err := (AgentExtractor{LLM: fake}).ExtractEntityFacts(ctx, input)
	if err != nil {
		t.Fatalf("ExtractEntityFacts error = %v", err)
	}
	if len(out.Entities) != 1 || out.Entities[0].Name != "Ada" {
		t.Fatalf("entities = %+v, want only explicit Ada entity", out.Entities)
	}
	if len(out.Facts) != 0 {
		t.Fatalf("facts = %+v, want missing subject/object proposals dropped", out.Facts)
	}
}

func TestAgentExtractorValidatesObjectSpansAndGraphableMetadata(t *testing.T) {
	ctx := context.Background()
	fake := &agentFakeLLM{replies: []string{`{
		"entities":[{"name":"Ada","type":"person","source_ids":["dia-1"],"confidence":0.9}],
		"facts":[]
	}`, `{
		"entities":[],
		"facts":[{"subject":"Ada","relation_type":"preference","predicate_text":"likes","object_spans":[{"text":"green tea","source_id":"dia-1","type":"literal"},{"text":"oolong","source_id":"dia-1","type":"literal"},{"text":"green tea","source_id":"dia-2","type":"literal"}],"fact_text":"Ada likes green tea.","source_ids":["dia-1"],"confidence":0.95}]
	}`}}
	input := agentEntityFactInput([]sourcemessage.Message{
		{ID: "dia-1", ConversationID: "conv", Seq: 1, Message: model.NewTextMessage(model.RoleUser, "Ada likes green tea.")},
	})

	out, err := (AgentExtractor{LLM: fake}).ExtractEntityFacts(ctx, input)
	if err != nil {
		t.Fatalf("ExtractEntityFacts error = %v", err)
	}
	if len(out.Facts) != 1 {
		t.Fatalf("facts len = %d, want one fact", len(out.Facts))
	}
	fact := out.Facts[0]
	spans := viewentityfact.ObjectSpansFromMetadata(fact.Metadata)
	if len(spans) != 1 || spans[0].Text != "green tea" || spans[0].SourceID != "dia-1" || spans[0].Type != "literal" {
		t.Fatalf("object spans = %+v, want only exact source-backed span", spans)
	}
	if got := fact.Metadata[viewentityfact.FactGraphableMetadataKey]; got != true || !viewentityfact.IsGraphableFact(fact) {
		t.Fatalf("graphable metadata = %v, want true for source-backed object span", got)
	}
}

func TestAgentExtractorRepairsUngraphableSourceCoverage(t *testing.T) {
	ctx := context.Background()
	fake := &agentFakeLLM{replies: []string{`{
		"entities":[{"name":"Ada","type":"person","source_ids":["dia-1"],"confidence":0.9}],
		"facts":[]
	}`, `{
		"entities":[],
		"facts":[{"subject":"Ada","relation_type":"other","predicate_text":"mentioned","fact_text":"Ada mentioned sculpture.","source_ids":["dia-1"],"confidence":0.8}]
	}`, `{
		"entities":[],
		"facts":[{"subject":"Ada","relation_type":"activity","predicate_text":"practices","object_spans":[{"text":"sculpture","source_id":"dia-1","type":"object"}],"fact_text":"Ada practices sculpture.","source_ids":["dia-1"],"confidence":0.9}]
	}`}}
	input := agentEntityFactInput([]sourcemessage.Message{
		{ID: "dia-1", ConversationID: "conv", Seq: 1, Message: model.NewTextMessage(model.RoleUser, "Ada practices sculpture.")},
	})

	out, err := (AgentExtractor{LLM: fake}).ExtractEntityFacts(ctx, input)
	if err != nil {
		t.Fatalf("ExtractEntityFacts error = %v", err)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("fake calls = %d, want entity, fact, and repair calls", len(fake.calls))
	}
	if len(out.Facts) != 2 {
		t.Fatalf("facts = %+v, want original weak fact plus repaired graphable fact", out.Facts)
	}
	repaired := out.Facts[1]
	if repaired.RelationType != viewentityfact.RelationActivity || !viewentityfact.IsGraphableFact(repaired) {
		t.Fatalf("repaired fact = %+v, want graphable activity fact", repaired)
	}
	spans := viewentityfact.ObjectSpansFromMetadata(repaired.Metadata)
	if len(spans) != 1 || spans[0].Text != "sculpture" || spans[0].SourceID != "dia-1" {
		t.Fatalf("repaired spans = %+v, want exact source-backed sculpture span", spans)
	}
	repairControl := fake.calls[2].messages[1].Content()
	if !strings.Contains(repairControl, "conversation_source_message_fact_coverage_repair") || !strings.Contains(repairControl, "entity_catalog") {
		t.Fatalf("repair control = %s, want coverage repair task with entity catalog", repairControl)
	}
}

func TestAgentExtractorRepairDoesNotCreateImplicitObjectEntities(t *testing.T) {
	ctx := context.Background()
	fake := &agentFakeLLM{replies: []string{`{
		"entities":[{"name":"Ada","type":"person","source_ids":["dia-1"],"confidence":0.9}],
		"facts":[]
	}`, `{
		"entities":[],
		"facts":[]
	}`, `{
		"entities":[],
		"facts":[{"subject":"Ada","relation_type":"preference","predicate_text":"likes","object_names":["tea"],"fact_text":"Ada likes tea.","source_ids":["dia-1"],"confidence":0.9}]
	}`}}
	input := agentEntityFactInput([]sourcemessage.Message{
		{ID: "dia-1", ConversationID: "conv", Seq: 1, Message: model.NewTextMessage(model.RoleUser, "Ada likes tea.")},
	})

	out, err := (AgentExtractor{LLM: fake}).ExtractEntityFacts(ctx, input)
	if err != nil {
		t.Fatalf("ExtractEntityFacts error = %v", err)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("fake calls = %d, want repair attempted", len(fake.calls))
	}
	if len(out.Entities) != 1 || out.Entities[0].Name != "Ada" {
		t.Fatalf("entities = %+v, want only Ada entity", out.Entities)
	}
	if len(out.Facts) != 0 {
		t.Fatalf("facts = %+v, want repair fact with unresolved object dropped", out.Facts)
	}
}

func TestAgentExtractorSkipsRepairForGraphCoveredMessages(t *testing.T) {
	ctx := context.Background()
	fake := &agentFakeLLM{replies: []string{`{
		"entities":[{"name":"Ada","type":"person","source_ids":["dia-1"],"confidence":0.9}],
		"facts":[]
	}`, `{
		"entities":[],
		"facts":[{"subject":"Ada","relation_type":"activity","predicate_text":"practices","object_spans":[{"text":"sculpture","source_id":"dia-1","type":"object"}],"fact_text":"Ada practices sculpture.","source_ids":["dia-1"],"confidence":0.9}]
	}`, `{
		"entities":[],
		"facts":[{"subject":"Ada","relation_type":"activity","predicate_text":"repairs","object_spans":[{"text":"bikes","source_id":"dia-1","type":"object"}],"fact_text":"Ada repairs bikes.","source_ids":["dia-1"],"confidence":0.9}]
	}`}}
	input := agentEntityFactInput([]sourcemessage.Message{
		{ID: "dia-1", ConversationID: "conv", Seq: 1, Message: model.NewTextMessage(model.RoleUser, "Ada practices sculpture.")},
	})

	out, err := (AgentExtractor{LLM: fake}).ExtractEntityFacts(ctx, input)
	if err != nil {
		t.Fatalf("ExtractEntityFacts error = %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("fake calls = %d, want no repair call when source is graph-covered", len(fake.calls))
	}
	if len(out.Facts) != 1 || !viewentityfact.IsGraphableFact(out.Facts[0]) {
		t.Fatalf("facts = %+v, want one graphable fact", out.Facts)
	}
}

func TestAgentExtractorRepairFailureDoesNotFailExtraction(t *testing.T) {
	ctx := context.Background()
	fake := &agentFakeLLM{replies: []string{`{
		"entities":[{"name":"Ada","type":"person","source_ids":["dia-1"],"confidence":0.9}],
		"facts":[]
	}`, `{
		"entities":[],
		"facts":[]
	}`, `not-json`}}
	input := agentEntityFactInput([]sourcemessage.Message{
		{ID: "dia-1", ConversationID: "conv", Seq: 1, Message: model.NewTextMessage(model.RoleUser, "Ada likes tea.")},
	})

	out, err := (AgentExtractor{LLM: fake}).ExtractEntityFacts(ctx, input)
	if err != nil {
		t.Fatalf("ExtractEntityFacts error = %v", err)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("fake calls = %d, want repair attempted", len(fake.calls))
	}
	if len(out.Entities) != 1 || len(out.Facts) != 0 {
		t.Fatalf("output = %+v, want entity retained and failed repair ignored", out)
	}
}

func TestAgentExtractorSkipsCoveredMessages(t *testing.T) {
	ctx := context.Background()
	fake := &agentFakeLLM{reply: `{"entities":[],"facts":[]}`}
	msgs := []sourcemessage.Message{
		{ID: "dia-1", ConversationID: "conv", Seq: 1, Message: model.NewTextMessage(model.RoleUser, "covered")},
	}
	ref := views.SourceRef{Kind: views.SourceMessage, Message: &views.MessageSourceRef{ConversationID: "conv", MessageID: "dia-1"}}
	input := agentEntityFactInput(msgs)
	input.CurrentFacts = []viewentityfact.Fact{{
		ID:              "fact_existing",
		Scope:           input.Scope,
		SubjectEntityID: "ent_existing",
		RelationType:    viewentityfact.RelationOther,
		FactText:        "covered",
		SourceRefs:      []views.SourceRef{ref},
	}}

	out, err := (AgentExtractor{LLM: fake}).ExtractEntityFacts(ctx, input)
	if err != nil {
		t.Fatalf("ExtractEntityFacts error = %v", err)
	}
	if len(out.Entities) != 0 || len(out.Facts) != 0 || len(fake.calls) != 0 {
		t.Fatalf("output = %+v calls=%d, want no extraction for covered messages", out, len(fake.calls))
	}
}

type agentFakeLLM struct {
	reply   string
	replies []string
	calls   []agentFakeLLMCall
}

type agentFakeLLMCall struct {
	messages []sdkllm.Message
	jsonMode *bool
}

func (f *agentFakeLLM) Generate(_ context.Context, messages []sdkllm.Message, opts ...sdkllm.GenerateOption) (sdkllm.Message, sdkllm.TokenUsage, error) {
	applied := sdkllm.ApplyOptions(opts...)
	reply := f.reply
	if len(f.replies) > len(f.calls) {
		reply = f.replies[len(f.calls)]
	}
	f.calls = append(f.calls, agentFakeLLMCall{
		messages: append([]sdkllm.Message(nil), messages...),
		jsonMode: applied.JSONMode,
	})
	return sdkllm.NewTextMessage(sdkllm.RoleAssistant, reply), sdkllm.TokenUsage{}, nil
}

func (f *agentFakeLLM) GenerateStream(context.Context, []sdkllm.Message, ...sdkllm.GenerateOption) (sdkllm.StreamMessage, error) {
	return nil, nil
}

func agentEntityFactInput(messages []sourcemessage.Message) derive.EntityFactInput {
	scope := views.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	refs := make([]views.SourceRef, 0, len(messages))
	for _, msg := range messages {
		refs = append(refs, views.SourceRef{Kind: views.SourceMessage, Message: &views.MessageSourceRef{ConversationID: msg.ConversationID, MessageID: msg.ID}})
	}
	return derive.EntityFactInput{
		View:  views.Descriptor{ID: viewentityfact.DefaultEntityFactsID, Kind: views.KindEntityFacts, Version: viewentityfact.DefaultEntityFactsVersion},
		Scope: scope,
		Window: recent.WindowResult{
			Messages:   messages,
			SourceRefs: refs,
		},
	}
}
