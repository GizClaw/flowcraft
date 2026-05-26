package flowcraftv2

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

var (
	_ runners.AnswerContextRecaller     = (*Runner)(nil)
	_ runners.AnswerContextStageAuditor = (*Runner)(nil)
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

func TestBuildTurnContexts_ParsesLongMemEvalTimestamp(t *testing.T) {
	ctxs, observedAt := buildTurnContexts([]runners.RawTurn{{
		Role:       "user",
		Content:    "[2023/04/10 (Mon) 17:50] user: I just got my car serviced.",
		EvidenceID: "q1:s1:t0",
		SessionID:  "q1:s1",
	}}, false)
	if len(ctxs) != 1 {
		t.Fatalf("expected 1 typed turn, got %d", len(ctxs))
	}
	got := ctxs[0]
	if got.Text != "I just got my car serviced." {
		t.Fatalf("text should be stripped of LongMemEval prefix, got %q", got.Text)
	}
	want := time.Date(2023, 4, 10, 17, 50, 0, 0, time.UTC)
	if !got.Time.Equal(want) {
		t.Fatalf("typed Time = %v, want %v", got.Time, want)
	}
	if !observedAt.Equal(want) {
		t.Fatalf("observedAt = %v, want %v", observedAt, want)
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
		Name: "flowcraft-recall-v2",
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

	rendered := groundedHitContent(h)
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

func TestFromRecallArtifactUsesSelectedEvidence(t *testing.T) {
	artifact := fromRecallArtifact(recall.Hit{
		Fact: recall.TemporalFact{
			ID:      "f1",
			Content: "Alice visited Paris.",
			EvidenceRefs: []recall.EvidenceRef{
				{ID: "D1:1", Text: "Alice mentioned Paris."},
				{ID: "D1:2", Text: "Unrelated nearby turn."},
			},
		},
		Evidence: []recall.EvidenceRef{{ID: "D1:1", Text: "Alice mentioned Paris."}},
	})
	if !strings.Contains(artifact.Content, "Alice mentioned Paris.") {
		t.Fatalf("selected evidence missing from rendered content: %+v", artifact)
	}
	if strings.Contains(artifact.Content, "Unrelated nearby turn") {
		t.Fatalf("unselected evidence should not be rendered: %+v", artifact)
	}
	if len(artifact.EvidenceIDs) != 1 || artifact.EvidenceIDs[0] != "D1:1" {
		t.Fatalf("artifact evidence ids should reflect selected evidence: %+v", artifact)
	}
}

func TestFromRecallArtifactUsesSupportingEvidenceRefs(t *testing.T) {
	artifact := fromRecallArtifact(recall.Hit{
		Fact: recall.TemporalFact{
			ID:      "f1",
			Content: "Caroline moved from her home country.",
			EvidenceRefs: []recall.EvidenceRef{
				{ID: "D1:1", Text: "Caroline moved from her home country four years ago."},
				{ID: "D1:2", Text: "Caroline said Sweden is where she moved from."},
				{ID: "D1:3", Text: "Unselected unrelated turn."},
			},
		},
		Evidence: []recall.EvidenceRef{
			{ID: "D1:1", Text: "Caroline moved from her home country four years ago."},
			{ID: "D1:2", Text: "Caroline said Sweden is where she moved from."},
		},
	})
	if !strings.Contains(artifact.Content, "Caroline moved from her home country four years ago.") {
		t.Fatalf("selected evidence missing from rendered content: %+v", artifact)
	}
	if !strings.Contains(artifact.Content, "Caroline said Sweden is where she moved from.") {
		t.Fatalf("supporting evidence missing from rendered content: %+v", artifact)
	}
	if strings.Contains(artifact.Content, "Unselected unrelated turn") {
		t.Fatalf("unselected evidence should not be rendered: %+v", artifact)
	}
	if got, want := strings.Join(artifact.EvidenceIDs, ","), "D1:1,D1:2"; got != want {
		t.Fatalf("artifact evidence ids = %s, want %s", got, want)
	}
}

func TestGroundedHitContentAnnotatesEvidenceSourceTime(t *testing.T) {
	hit := groundedHitContent(recall.Hit{
		Fact: recall.TemporalFact{
			ID:        "f1",
			Content:   "Alice plans to visit Tampa next month.",
			ValidFrom: ptrTime(time.Date(2024, 6, 7, 0, 0, 0, 0, time.UTC)),
		},
		Evidence: []recall.EvidenceRef{{
			ID:        "D1:7",
			Text:      "I am visiting Tampa next month.",
			Timestamp: time.Date(2024, 5, 7, 9, 30, 0, 0, time.UTC),
		}},
	})
	if !strings.Contains(hit, "[time: 2024-06-07]") {
		t.Fatalf("resolved fact time missing from rendered content: %s", hit)
	}
	if !strings.Contains(hit, "[source_time: 2024-05-07 09:30] I am visiting Tampa next month.") {
		t.Fatalf("source time missing from rendered evidence: %s", hit)
	}
}

func TestGroundedHitContentRendersSourceTimeFallbackAsObservedAt(t *testing.T) {
	validFrom := time.Date(2024, 5, 7, 9, 30, 0, 0, time.UTC)
	h := recall.Hit{
		Fact: recall.TemporalFact{
			ID:        "f1",
			Content:   "Alice mentioned Tampa.",
			ValidFrom: &validFrom,
			Metadata: map[string]any{
				"valid_from_source": "source_time_fallback",
			},
		},
		Evidence: []recall.EvidenceRef{{
			ID:        "D1:7",
			Text:      "I mentioned Tampa.",
			Timestamp: validFrom,
		}},
	}
	content := groundedHitContent(h)
	if strings.Contains(content, "[time: 2024-05-07]") {
		t.Fatalf("source-time fallback must not render as event time: %s", content)
	}
	if !strings.Contains(content, "[observed_at: 2024-05-07]") {
		t.Fatalf("source-time fallback should render as weak observed_at anchor: %s", content)
	}
	if !strings.Contains(content, "[source_time: 2024-05-07 09:30] I mentioned Tampa.") {
		t.Fatalf("source time evidence should remain visible: %s", content)
	}
	if got := fromRecallArtifact(h).ValidFrom; got != "" {
		t.Fatalf("runner ValidFrom should be empty for source-time fallback, got %q", got)
	}
}

func TestStructuredAnswerBodyKeepsRecallHitStructure(t *testing.T) {
	eventTime := time.Date(2023, 7, 14, 0, 0, 0, 0, time.UTC)
	observedAt := time.Date(2023, 7, 15, 13, 51, 0, 0, time.UTC)
	body := renderStructuredAnswerBody(runners.AnswerQuestion{
		Query:   "When did Melanie go to the pottery workshop?",
		AskedAt: "2023-10-01",
	}, []recall.Hit{{
		Fact: recall.TemporalFact{
			ID:         "f1",
			Kind:       recall.FactEvent,
			Content:    "Last Friday, Melanie took her kids to a pottery workshop.",
			Subject:    "Melanie",
			Predicate:  "went_to",
			Object:     "pottery workshop",
			ObservedAt: observedAt,
			ValidFrom:  &eventTime,
			Metadata: map[string]any{
				"valid_from_source": "content_relative",
				"valid_from_text":   "Last Friday",
			},
		},
		Score:   0.42,
		Sources: []string{"retrieval", "timeline"},
		Evidence: []recall.EvidenceRef{{
			ID:        "conv-26:D8:2",
			Role:      "user",
			Text:      "Last Fri I finally took the kids to a pottery workshop.",
			Timestamp: observedAt,
		}},
	}})

	for _, want := range []string{
		"ASKED_AT: 2023-10-01",
		"QUESTION: When did Melanie go to the pottery workshop?",
		"MEMORIES (STRUCTURED_FACTS):",
		`fact_id: "f1"`,
		`kind: "event"`,
		`content: "Last Friday, Melanie took her kids to a pottery workshop."`,
		`event_time: "2023-07-14"`,
		`event_time_source: "content_relative"`,
		`event_time_text: "Last Friday"`,
		`observed_at: "2023-07-15 13:51"`,
		`source_time: "2023-07-15 13:51"`,
		`quote: "Last Fri I finally took the kids to a pottery workshop."`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "| evidence:") {
		t.Fatalf("structured answer body should not use flattened evidence rendering:\n%s", body)
	}
}

func TestStructuredAnswerBodyDoesNotPromoteSourceTimeFallbackToEventTime(t *testing.T) {
	validFrom := time.Date(2024, 5, 7, 9, 30, 0, 0, time.UTC)
	body := renderStructuredAnswerBody(runners.AnswerQuestion{
		Query: "When did Alice mention Tampa?",
	}, []recall.Hit{{
		Fact: recall.TemporalFact{
			ID:         "f1",
			Kind:       recall.FactState,
			Content:    "Alice mentioned Tampa.",
			ObservedAt: validFrom,
			ValidFrom:  &validFrom,
			Metadata: map[string]any{
				"valid_from_source": "source_time_fallback",
				"valid_from_text":   "2024-05-07T09:30:00Z",
			},
		},
		Evidence: []recall.EvidenceRef{{
			ID:        "D1:7",
			Text:      "I mentioned Tampa.",
			Timestamp: validFrom,
		}},
	}})

	for _, unwanted := range []string{
		`event_time: "2024-05-07"`,
		`event_time_source: "source_time_fallback"`,
		`event_time_text: "2024-05-07T09:30:00Z"`,
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("source-time fallback must not render %q:\n%s", unwanted, body)
		}
	}
	for _, want := range []string{
		`observed_at: "2024-05-07 09:30"`,
		`source_time: "2024-05-07 09:30"`,
		`quote: "I mentioned Tampa."`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
}

func TestStructuredAnswerContextCarriesBackendPrompt(t *testing.T) {
	ctx := structuredAnswerContext(runners.AnswerQuestion{Query: "What happened?"}, nil)
	if ctx.Format != "flowcraftv2_structured_facts" {
		t.Fatalf("unexpected format: %q", ctx.Format)
	}
	for _, want := range []string{
		"structured memory facts",
		"event_time as the event date",
		"observed_at and evidence source_time",
		"content as the canonical extracted fact",
		"best-supported yes/no inference",
		"%s",
	} {
		if !strings.Contains(ctx.PromptTemplate, want) {
			t.Fatalf("structured prompt missing %q:\n%s", want, ctx.PromptTemplate)
		}
	}
}

func ptrTime(t time.Time) *time.Time {
	return &t
}
