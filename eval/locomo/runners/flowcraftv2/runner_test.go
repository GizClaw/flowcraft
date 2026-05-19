package flowcraftv2

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall"
)

type fakeLLM struct {
	response string
}

func (f fakeLLM) Generate(context.Context, []llm.Message, ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if f.response == "" {
		return llm.NewTextMessage(llm.RoleAssistant, `{"facts":[]}`), llm.TokenUsage{}, nil
	}
	return llm.NewTextMessage(llm.RoleAssistant, f.response), llm.TokenUsage{}, nil
}

func (fakeLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("fakeLLM: streaming not implemented")
}

func TestRenderTurnsJSONLIncludesEvidenceAndSession(t *testing.T) {
	text, n := renderTurns([]runners.RawTurn{
		{Role: "user", Content: "Alice likes Paris.", EvidenceID: "D1:3", SessionID: "session_2"},
		{Role: "assistant", Content: "Nice!", EvidenceID: "D1:4", SessionID: "session_2"},
	}, false)
	if n != 1 {
		t.Fatalf("rendered %d turns, want 1", n)
	}
	if !strings.Contains(text, "FLOWCRAFT_RECALL_TURNS_V1") {
		t.Fatalf("missing render header: %q", text)
	}
	for _, want := range []string{`"turn_id":"D1:3"`, `"evidence_id":"D1:3"`, `"session_id":"session_2"`, `"role":"user"`, `"text":"Alice likes Paris."`} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered turns missing %s in %q", want, text)
		}
	}
	if strings.Contains(text, "D1:4") {
		t.Fatalf("assistant turn should be skipped when IncludeAssistant=false: %q", text)
	}
}

func TestSaveSourceTurnsPersistsExtractorEvidenceRefs(t *testing.T) {
	r, err := New(Options{
		Name: "flowcraft-v2",
		LLM: fakeLLM{response: `{"facts":[{
			"kind":"preference",
			"subject":"alice",
			"predicate":"city",
			"content":"Paris",
			"source_message_ids":["D1:3"],
			"evidence_refs":[{"id":"D1:3","message_id":"D1:3","role":"user","text":"Alice likes Paris."}]
		}]}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	saver, ok := r.(runners.SourceTurnSaver)
	if !ok {
		t.Fatal("flowcraftv2 runner must support source-turn extractor ingest")
	}
	scope := runners.Scope{RuntimeID: "rt", UserID: "u1"}
	n, _, err := saver.SaveSourceTurns(context.Background(), scope, []runners.RawTurn{{
		Role: "user", Content: "Alice likes Paris.", EvidenceID: "D1:3", SessionID: "session_2",
	}})
	if err != nil {
		t.Fatalf("save source turns: %v", err)
	}
	if n != 1 {
		t.Fatalf("saved %d facts, want 1", n)
	}

	hits, _, err := r.Recall(context.Background(), scope, "Paris", 5)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	if len(hits[0].EvidenceIDs) != 1 || hits[0].EvidenceIDs[0] != "D1:3" {
		t.Fatalf("evidence ids not preserved in hit: %+v", hits[0])
	}
	if !strings.Contains(hits[0].Content, "Alice likes Paris.") {
		t.Fatalf("grounded hit content should include evidence text: %+v", hits[0])
	}
}

func TestGroundedHitContentSatisfiesAnswerContextDiagnostics(t *testing.T) {
	h := recall.Hit{Fact: recall.TemporalFact{
		ID:      "f1",
		Content: "Caroline joined a support group",
		EvidenceRefs: []recall.EvidenceRef{{
			ID:   "D1:3",
			Text: "[9:00 am on 7 May, 2024] Caroline went to the LGBTQ support group.",
		}},
	}}

	rendered := groundedHitContent(h.Fact)
	if attrs := recall.AttributeAnswerContext([]recall.Hit{h}, []recall.AnswerContextItem{{
		FactID: h.Fact.ID,
		Text:   h.Fact.Content,
	}}); len(attrs) == 0 {
		t.Fatal("diagnostics should flag content-only rendering")
	}
	if attrs := recall.AttributeAnswerContext([]recall.Hit{h}, []recall.AnswerContextItem{{
		FactID: h.Fact.ID,
		Text:   rendered,
	}}); len(attrs) != 0 {
		t.Fatalf("grounded rendering should satisfy diagnostics, got %+v", attrs)
	}
}
