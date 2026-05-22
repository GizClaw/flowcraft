package flowcraftv2

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall"
	"github.com/GizClaw/flowcraft/sdk/recall/diagnostics"
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

func TestBuildTurnContexts_FiltersAssistantAndPreservesEvidence(t *testing.T) {
	ctxs, observedAt := buildTurnContexts([]runners.RawTurn{
		{Role: "user", Content: "Alice likes Paris.", EvidenceID: "D1:3", SessionID: "session_2"},
		{Role: "assistant", Content: "Nice!", EvidenceID: "D1:4", SessionID: "session_2"},
	}, false)
	if len(ctxs) != 1 {
		t.Fatalf("expected 1 typed turn, got %d", len(ctxs))
	}
	got := ctxs[0]
	if got.EvidenceID != "D1:3" || got.ID != "D1:3" {
		t.Errorf("turn id mismatch: id=%q evidence=%q", got.ID, got.EvidenceID)
	}
	if got.SessionID != "session_2" || got.Role != "user" {
		t.Errorf("session/role not preserved: %+v", got)
	}
	if got.Text != "Alice likes Paris." {
		t.Errorf("text not preserved: %q", got.Text)
	}
	if !observedAt.IsZero() {
		t.Errorf("no typed timestamps -> observedAt should be zero, got %v", observedAt)
	}
}

// TestBuildTurnContexts_LiftsTimestampAndSpeaker pins the typed-channel
// contract: LoCoMo bakes "[<time>] <speaker>: <body>" into the turn
// content, and the runner must strip that prefix into typed Time /
// Speaker fields so the LLM no longer has to grep timestamps out of
// prose. The body that downstream consumers see must contain ONLY
// the spoken text.
func TestBuildTurnContexts_LiftsTimestampAndSpeaker(t *testing.T) {
	ctxs, observedAt := buildTurnContexts([]runners.RawTurn{{
		Role:       "user",
		Content:    "[11:30 pm on 21 January, 2023] Melanie: I signed up yesterday.",
		EvidenceID: "D1:7",
		SessionID:  "session_3",
	}}, false)
	if len(ctxs) != 1 {
		t.Fatalf("expected 1 typed turn, got %d", len(ctxs))
	}
	got := ctxs[0]
	if got.Speaker != "Melanie" {
		t.Errorf("speaker = %q, want Melanie", got.Speaker)
	}
	if got.Text != "I signed up yesterday." {
		t.Errorf("text should be stripped of prefix, got %q", got.Text)
	}
	if got.Time.IsZero() {
		t.Errorf("typed Time should be parsed from prefix, got zero")
	}
	if observedAt != got.Time {
		t.Errorf("observedAt = %v, want %v", observedAt, got.Time)
	}
}

// TestBuildTurnContexts_DegradesForRawChat covers adapters that
// don't bake a [<time>] <speaker>: prefix into content (synthetic
// data, raw chat dumps): Time stays zero and Speaker empty so the
// typed channel is opt-in per adapter, and text remains verbatim.
func TestBuildTurnContexts_DegradesForRawChat(t *testing.T) {
	ctxs, observedAt := buildTurnContexts([]runners.RawTurn{{
		Role:    "user",
		Content: "Alice likes Paris.",
	}}, false)
	if len(ctxs) != 1 {
		t.Fatalf("expected 1 typed turn, got %d", len(ctxs))
	}
	got := ctxs[0]
	if !got.Time.IsZero() || got.Speaker != "" {
		t.Errorf("raw chat should not synthesize typed time/speaker: %+v", got)
	}
	if got.Text != "Alice likes Paris." {
		t.Errorf("text should pass through verbatim, got %q", got.Text)
	}
	if !observedAt.IsZero() {
		t.Errorf("observedAt should be zero for prefix-less content, got %v", observedAt)
	}
}

func TestSaveSourceTurnsPersistsExtractorEvidenceRefs(t *testing.T) {
	r, err := New(Options{
		Name: "flowcraft-v2",
		LLM: fakeLLM{response: `{"memories":[{
			"text":"Alice likes Paris.",
			"evidence_refs":[{"id":"D1:3","text":"Alice likes Paris."}]
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

	auditor, ok := r.(runners.RecallStageAuditor)
	if !ok {
		t.Fatal("flowcraftv2 runner must support recall stage audits")
	}
	_, audit, _, err := auditor.RecallWithStageAudit(context.Background(), scope, "Paris", 5)
	if err != nil {
		t.Fatalf("recall audit: %v", err)
	}
	if len(audit.Stages) == 0 {
		t.Fatal("expected stage audit snapshots")
	}
	var sawSource, sawRank, sawHits bool
	for _, st := range audit.Stages {
		if st.Stage == "source_fanout" && len(st.Candidates) > 0 {
			sawSource = true
		}
		if st.Stage == "rank_output" && len(st.Candidates) > 0 {
			sawRank = true
		}
		if st.Stage == "build_hits" && len(st.Candidates) > 0 {
			sawHits = true
		}
	}
	if !sawSource || !sawRank || !sawHits {
		t.Fatalf("missing expected source/rank/hits snapshots: %+v", audit.Stages)
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
	if attrs := diagnostics.AttributeAnswerContext([]recall.Hit{h}, []diagnostics.AnswerContextItem{{
		FactID: h.Fact.ID,
		Text:   h.Fact.Content,
	}}); len(attrs) == 0 {
		t.Fatal("diagnostics should flag content-only rendering")
	}
	if attrs := diagnostics.AttributeAnswerContext([]recall.Hit{h}, []diagnostics.AnswerContextItem{{
		FactID: h.Fact.ID,
		Text:   rendered,
	}}); len(attrs) != 0 {
		t.Fatalf("grounded rendering should satisfy diagnostics, got %+v", attrs)
	}
}
