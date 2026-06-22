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

func TestLLMExtractorMaterializesStableEntitiesAndFacts(t *testing.T) {
	ctx := context.Background()
	fake := &entityFakeLLM{reply: `{
		"entities":[{"name":"Ada","type":"person","aliases":["Ada L."],"source_ids":["dia-1"],"confidence":0.9}],
		"facts":[{"subject":"Ada","relation_type":"preference","predicate_text":"likes","object_names":["tea"],"fact_text":"Ada likes tea.","source_ids":["dia-1"],"confidence":0.95}]
	}`}
	input := entityFactInput([]sourcemessage.Message{
		{ID: "dia-1", ConversationID: "conv", Seq: 1, Message: model.NewTextMessage(model.RoleUser, "Ada likes tea.")},
	})

	out, err := (LLMExtractor{LLM: fake}).ExtractEntityFacts(ctx, input)
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
	if fact.RelationType != viewentityfact.RelationPreference || fact.FactText != "Ada likes tea." {
		t.Fatalf("fact = %+v, want preference fact text", fact)
	}
	if len(fact.SourceRefs) != 1 || fact.SourceRefs[0].Message == nil || fact.SourceRefs[0].Message.MessageID != "dia-1" {
		t.Fatalf("fact SourceRefs = %+v, want dia-1", fact.SourceRefs)
	}
	if len(fake.calls) != 1 || fake.calls[0].jsonMode == nil || !*fake.calls[0].jsonMode {
		t.Fatalf("fake calls = %+v, want one JSON-mode call", fake.calls)
	}
	if got := len(fake.calls[0].messages); got != 3 {
		t.Fatalf("LLM messages len = %d, want system + control + source message", got)
	}
	control := fake.calls[0].messages[1].Content()
	if !strings.Contains(control, "dia-1") || strings.Contains(control, "Ada likes tea") {
		t.Fatalf("control = %s, want source ids without flattened source text", control)
	}
	sourceMsg := fake.calls[0].messages[2]
	if got := sourceMsg.Content(); got != "Ada likes tea." {
		t.Fatalf("source message content = %q, want original evidence text", got)
	}
	metadata := promptSourceMetadata(t, sourceMsg)
	if metadata["source_id"] != "dia-1" || metadata["conversation_id"] != "conv" {
		t.Fatalf("source metadata = %+v, want source id and conversation id", metadata)
	}
}

func TestLLMExtractorSkipsCoveredMessages(t *testing.T) {
	ctx := context.Background()
	fake := &entityFakeLLM{reply: `{"entities":[],"facts":[]}`}
	msgs := []sourcemessage.Message{
		{ID: "dia-1", ConversationID: "conv", Seq: 1, Message: model.NewTextMessage(model.RoleUser, "covered")},
	}
	ref := views.SourceRef{Kind: views.SourceMessage, Message: &views.MessageSourceRef{ConversationID: "conv", MessageID: "dia-1"}}
	input := entityFactInput(msgs)
	input.CurrentFacts = []viewentityfact.Fact{{
		ID:              "fact_existing",
		Scope:           input.Scope,
		SubjectEntityID: "ent_existing",
		RelationType:    viewentityfact.RelationOther,
		FactText:        "covered",
		SourceRefs:      []views.SourceRef{ref},
	}}

	out, err := (LLMExtractor{LLM: fake}).ExtractEntityFacts(ctx, input)
	if err != nil {
		t.Fatalf("ExtractEntityFacts error = %v", err)
	}
	if len(out.Entities) != 0 || len(out.Facts) != 0 || len(fake.calls) != 0 {
		t.Fatalf("output = %+v calls=%d, want no extraction for covered messages", out, len(fake.calls))
	}
}

type entityFakeLLM struct {
	reply string
	calls []entityFakeLLMCall
}

type entityFakeLLMCall struct {
	messages []sdkllm.Message
	jsonMode *bool
}

func (f *entityFakeLLM) Generate(_ context.Context, messages []sdkllm.Message, opts ...sdkllm.GenerateOption) (sdkllm.Message, sdkllm.TokenUsage, error) {
	applied := sdkllm.ApplyOptions(opts...)
	f.calls = append(f.calls, entityFakeLLMCall{
		messages: append([]sdkllm.Message(nil), messages...),
		jsonMode: applied.JSONMode,
	})
	return sdkllm.NewTextMessage(sdkllm.RoleAssistant, f.reply), sdkllm.TokenUsage{}, nil
}

func (f *entityFakeLLM) GenerateStream(context.Context, []sdkllm.Message, ...sdkllm.GenerateOption) (sdkllm.StreamMessage, error) {
	return nil, nil
}

func promptSourceMetadata(t *testing.T, msg sdkllm.Message) map[string]any {
	t.Helper()
	if len(msg.Parts) == 0 {
		t.Fatalf("message has no parts: %+v", msg)
	}
	part := msg.Parts[len(msg.Parts)-1]
	if part.Type != model.PartData || part.Data == nil {
		t.Fatalf("last part = %+v, want source metadata data part", part)
	}
	if part.Data.MimeType != sourcemessage.PromptSourceMessageMIMEType {
		t.Fatalf("metadata MIME = %q, want %q", part.Data.MimeType, sourcemessage.PromptSourceMessageMIMEType)
	}
	return part.Data.Value
}

func entityFactInput(messages []sourcemessage.Message) derive.EntityFactInput {
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
