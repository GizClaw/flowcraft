package flowcraftv2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
	retrievalmem "github.com/GizClaw/flowcraft/memory/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

var (
	_ runners.AnswerContextRecaller     = (*Runner)(nil)
	_ runners.AnswerContextStageAuditor = (*Runner)(nil)
)

func TestNewRequiresRetrievalIndex(t *testing.T) {
	_, err := New(Options{Name: "flowcraft-recall-v2"})
	if !errors.Is(err, ErrRetrievalIndexRequired) {
		t.Fatalf("New error = %v, want %v", err, ErrRetrievalIndexRequired)
	}
}

type fakeLLM struct {
	response string
}

func (f fakeLLM) Generate(_ context.Context, _ []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	got := llm.GenerateOptions{}
	for _, opt := range opts {
		opt(&got)
	}
	if got.JSONSchema != nil && strings.Contains(got.JSONSchema.Name, "segment_classifier") {
		return llm.NewTextMessage(llm.RoleAssistant, `{"segments":[{"segment_id":"D1:3","families":["semantic_fact"]}]}`), llm.TokenUsage{}, nil
	}
	if f.response == "" {
		return llm.NewTextMessage(llm.RoleAssistant, `{"proposals":[]}`), llm.TokenUsage{}, nil
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

// TestBuildTurnContexts_UsesStructuredTimestampAndSpeaker pins the typed-channel
// contract: LoCoMo conversion carries speaker/time separately from Content, so
// the LLM sees clean speaker-authored text plus typed metadata.
func TestBuildTurnContexts_UsesStructuredTimestampAndSpeaker(t *testing.T) {
	ctxs, observedAt := buildTurnContexts([]runners.RawTurn{{
		Role:       "user",
		Content:    "I signed up yesterday.",
		Speaker:    "Melanie",
		Timestamp:  "11:30 pm on 21 January, 2023",
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

func TestBuildTurnContexts_LegacyPrefixFallback(t *testing.T) {
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
		t.Errorf("legacy prefix should be stripped, got %q", got.Text)
	}
	if got.Time.IsZero() || observedAt != got.Time {
		t.Errorf("typed Time/observedAt should be parsed from legacy prefix, got time=%v observed=%v", got.Time, observedAt)
	}
}

func TestBuildTurnContexts_RendersStructuredImagesAsVisualEvidence(t *testing.T) {
	ctxs, _ := buildTurnContexts([]runners.RawTurn{{
		Role:       "user",
		Content:    "Take a look at my new camera.",
		Speaker:    "Dave",
		Timestamp:  "10:54 am on 17 November, 2023",
		EvidenceID: "D30:5",
		SessionID:  "session_30",
		Images: []runners.RawImage{{
			URL:     "https://example/camera.jpg",
			Query:   "vintage camera",
			Caption: "a photo of a camera sitting on a table next to a plant",
		}},
	}}, false)
	if len(ctxs) != 1 {
		t.Fatalf("expected 1 typed turn, got %d", len(ctxs))
	}
	for _, want := range []string{
		"Take a look at my new camera.",
		"speaker_shared_image (image shared by the speaker in this turn; metadata is not quoted speech):",
		"query: vintage camera",
		"caption: a photo of a camera sitting on a table next to a plant",
		"url: https://example/camera.jpg",
	} {
		if !strings.Contains(ctxs[0].Text, want) {
			t.Fatalf("turn text missing %q:\n%s", want, ctxs[0].Text)
		}
	}
	if strings.Contains(ctxs[0].Text, "ATTACHED_IMAGE_METADATA") {
		t.Fatalf("turn text should not use legacy LoCoMo image marker:\n%s", ctxs[0].Text)
	}
}

func TestBuildTurnContexts_KeepsImageOnlyTurn(t *testing.T) {
	ctxs, _ := buildTurnContexts([]runners.RawTurn{{
		Role:      "user",
		Speaker:   "Dave",
		Timestamp: "10:54 am on 17 November, 2023",
		Images: []runners.RawImage{{
			Query:   "waterfall rocks",
			Caption: "a photo of rocks and a waterfall",
		}},
	}}, false)
	if len(ctxs) != 1 {
		t.Fatalf("expected image-only turn to be kept, got %d", len(ctxs))
	}
	if !strings.Contains(ctxs[0].Text, "speaker_shared_image") ||
		!strings.Contains(ctxs[0].Text, "caption: a photo of rocks and a waterfall") {
		t.Fatalf("image-only turn did not render metadata:\n%s", ctxs[0].Text)
	}
	if strings.Contains(ctxs[0].Text, "ATTACHED_IMAGE_METADATA") {
		t.Fatalf("image-only turn should not use legacy LoCoMo image marker:\n%s", ctxs[0].Text)
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

func TestStructuredAnswerContextUsesGenericRelativeTimeGuidance(t *testing.T) {
	ctx := structuredAnswerContext([]recall.Hit{{
		Fact: recall.TemporalFact{
			ID:      "fact-1",
			Content: "Melanie said she visited the pottery studio yesterday.",
		},
		Evidence: []recall.EvidenceRef{{
			ID:        "D8:4",
			Speaker:   "Melanie",
			Timestamp: time.Date(2023, 1, 21, 11, 30, 0, 0, time.UTC),
			Text:      "I visited the pottery studio yesterday.",
		}},
		Score: 0.9,
	}}, "temporal")

	for _, want := range []string{
		`<recall_strategy strategy="temporal">`,
		"relative temporal expression",
		"answer the supported part and name the missing detail",
	} {
		if !strings.Contains(ctx.Body+ctx.PromptTemplate, want) {
			t.Fatalf("answer context missing %q:\nbody:\n%s\nprompt:\n%s", want, ctx.Body, ctx.PromptTemplate)
		}
	}
	if strings.Contains(ctx.Body, "relative_time_hint") {
		t.Fatalf("answer context should not synthesize hard-matched temporal hints:\n%s", ctx.Body)
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
		Name:           "flowcraft-recall-v2",
		RetrievalIndex: retrievalmem.New(),
		LLM: fakeLLM{response: `{"proposals":[{
			"text":"Alice likes Paris.",
			"kind":"preference",
			"subject":"Alice",
			"predicate":"likes",
			"object":"Paris",
			"source_ids":["D1:3"],
			"quote":"Alice likes Paris."
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
	if len(hits) == 0 {
		t.Fatalf("hits = %+v", hits)
	}
	hit := hits[0]
	for _, candidate := range hits {
		if candidate.Kind != "observation" {
			hit = candidate
			break
		}
	}
	if !containsStringValue(hit.EvidenceIDs, "D1:3") {
		t.Fatalf("evidence ids not preserved in hit: %+v", hit)
	}
	if !strings.Contains(hit.Content, "Alice likes Paris.") {
		t.Fatalf("grounded hit content should include evidence text: %+v", hit)
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
	var sawIntent, sawSource, sawRank, sawHits bool
	for _, st := range audit.Stages {
		if st.Stage == "intent_route" && st.Query != nil {
			sawIntent = true
			if st.Query.Strategy == "" {
				t.Fatalf("intent route audit must include strategy: %+v", st.Query)
			}
		}
		if st.Stage == "candidate_fanout" && len(st.Candidates) > 0 {
			sawSource = true
		}
		if st.Stage == "rank_output" && len(st.Candidates) > 0 {
			sawRank = true
		}
		if st.Stage == "build_grounded_hits" && len(st.Candidates) > 0 {
			sawHits = true
		}
	}
	if !sawIntent || !sawSource || !sawRank || !sawHits {
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
	body := renderStructuredAnswerBody([]recall.Hit{{
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
			Speaker:   "Melanie",
			Text:      "Last Fri I finally took the kids to a pottery workshop.",
			Timestamp: observedAt,
		}},
	}}, "")

	for _, want := range []string{
		`<answer_candidates>`,
		`content_span: "Last Friday, Melanie took her kids to a pottery workshop."`,
		`object_span: "pottery workshop"`,
		`event_time_candidate: "2023-07-14"`,
		`event_time_precision: "day"`,
		`fact_id: "f1"`,
		`kind: "event"`,
		`content: "Last Friday, Melanie took her kids to a pottery workshop."`,
		`event_time: "2023-07-14"`,
		`event_time_source: "content_relative"`,
		`event_time_text: "Last Friday"`,
		`observed_at: "2023-07-15 13:51"`,
		`speaker: "Melanie"`,
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
	if strings.Contains(body, "EVIDENCE_PACKAGE") {
		t.Fatalf("plain TopK answer body should not render an evidence package:\n%s", body)
	}
}

func TestStructuredAnswerBodyDoesNotExposeNeighborCandidateSourceAsEvidencePackage(t *testing.T) {
	body := renderStructuredAnswerBody([]recall.Hit{{
		Fact:    recall.TemporalFact{ID: "f1", Kind: recall.FactState, Content: "Alice bought the necklace in Paris."},
		Sources: []string{"retrieval", "neighbor_candidate"},
	}}, "")
	for _, want := range []string{
		`fact_id: "f1"`,
		`content: "Alice bought the necklace in Paris."`,
		`sources: "retrieval"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "expanded_related_evidence") {
		t.Fatalf("expansion diagnostics must not affect answer package:\n%s", body)
	}
	if strings.Contains(body, "neighbor_candidate") {
		t.Fatalf("diagnostic-only expansion source must not render in answer body:\n%s", body)
	}
}

func TestStructuredAnswerBodyRendersAllTopKMemories(t *testing.T) {
	hits := make([]recall.Hit, 0, 14)
	for i := 0; i < 14; i++ {
		hits = append(hits, recall.Hit{
			Fact: recall.TemporalFact{
				ID:      fmt.Sprintf("f%d", i+1),
				Kind:    recall.FactState,
				Content: fmt.Sprintf("Alice keeps item %d.", i+1),
				Subject: "Alice",
			},
			Evidence: []recall.EvidenceRef{{ID: fmt.Sprintf("D%d:1", i+1), Text: fmt.Sprintf("Alice keeps item %d.", i+1)}},
		})
	}
	body := renderStructuredAnswerBody(hits, "")

	for _, want := range []string{
		`- [#1]`,
		`- [#14]`,
		`fact_id: "f14"`,
		`quote: "Alice keeps item 14."`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "additional recalled memories omitted") {
		t.Fatalf("plain TopK renderer should not omit recalled memories:\n%s", body)
	}
}

func TestStructuredAnswerBodyDoesNotRenderCoverageGroups(t *testing.T) {
	body := renderStructuredAnswerBody([]recall.Hit{
		{
			Fact: recall.TemporalFact{
				ID:        "cat",
				Kind:      recall.FactState,
				Content:   "Jordan has a cat named Bailey.",
				Subject:   "Jordan",
				Predicate: "has_pet",
				Object:    "Bailey",
			},
			Evidence: []recall.EvidenceRef{{ID: "D1:1", Text: "I have a cat named Bailey."}},
		},
		{
			Fact: recall.TemporalFact{
				ID:        "dog",
				Kind:      recall.FactState,
				Content:   "Jordan has a dog named Oliver.",
				Subject:   "Jordan",
				Predicate: "has_pet",
				Object:    "Oliver",
			},
			Evidence: []recall.EvidenceRef{{ID: "D2:1", Text: "Oliver is my dog."}},
		},
	}, "")
	for _, want := range []string{`fact_id: "cat"`, `fact_id: "dog"`, `object: "Bailey"`, `object: "Oliver"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "coverage_groups:") || strings.Contains(body, "ranked_memories:") {
		t.Fatalf("plain TopK renderer should not prepend coverage groups:\n%s", body)
	}
}

func TestStructuredAnswerBodyPackagesDirectAnswerCues(t *testing.T) {
	body := renderStructuredAnswerBody([]recall.Hit{{
		Fact: recall.TemporalFact{
			ID:        "f1",
			Kind:      recall.FactEvent,
			Content:   "Caroline attended an LGBTQ+ counseling workshop.",
			Subject:   "Caroline",
			Predicate: "attended",
			Object:    "LGBTQ+ counseling workshop",
		},
		Evidence: []recall.EvidenceRef{{
			ID:   "D4:13",
			Text: "Last Friday, I went to an LGBTQ+ counseling workshop.",
		}},
	}}, "")
	for _, want := range []string{
		`- [#1]`,
		`content: "Caroline attended an LGBTQ+ counseling workshop."`,
		`object: "LGBTQ+ counseling workshop"`,
		`quote: "Last Friday, I went to an LGBTQ+ counseling workshop."`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "candidate_answers:") || strings.Contains(body, `type: "direct_answer"`) {
		t.Fatalf("direct questions should not promote candidate_answers:\n%s", body)
	}
	if strings.Contains(body, "answer_cues:") {
		t.Fatalf("plain TopK renderer should not emit answer_cues:\n%s", body)
	}
}

func TestStructuredAnswerBodyKeepsSurfaceSpansInCuesOnly(t *testing.T) {
	body := renderStructuredAnswerBody([]recall.Hit{{
		Fact: recall.TemporalFact{
			ID:      "f1",
			Kind:    recall.FactState,
			Content: "Melanie loved reading \"Charlotte's Web\" as a kid.",
			Subject: "Melanie",
		},
		Evidence: []recall.EvidenceRef{{
			ID:   "D6:10",
			Text: "I loved reading \"Charlotte's Web\" as a kid.",
		}},
	}}, "")
	for _, want := range []string{
		`content: "Melanie loved reading \"Charlotte's Web\" as a kid."`,
		`quote: "I loved reading \"Charlotte's Web\" as a kid."`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{
		"candidate_answers:",
		`type: "surface_span"`,
		`source: "content_quote_span"`,
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("surface spans should remain cues, not candidate_answers %q:\n%s", unwanted, body)
		}
	}
}

func TestStructuredAnswerBodyDoesNotPromoteWhereCandidateAnswers(t *testing.T) {
	body := renderStructuredAnswerBody([]recall.Hit{{
		Fact: recall.TemporalFact{
			ID:       "f1",
			Kind:     recall.FactEvent,
			Content:  "Alice bought the necklace in Paris.",
			Subject:  "Alice",
			Object:   "necklace",
			Location: "Paris",
		},
		Evidence: []recall.EvidenceRef{{
			ID:   "D1:9",
			Text: "I bought the necklace in Paris.",
		}},
	}}, "")
	for _, want := range []string{
		`location: "Paris"`,
		`object: "necklace"`,
		`quote: "I bought the necklace in Paris."`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "candidate_answers:") || strings.Contains(body, `type: "location"`) {
		t.Fatalf("where direct questions should rely on answer_cues, not candidate_answers:\n%s", body)
	}
}

func TestStructuredAnswerBodyRendersStructuredEventTimeOnly(t *testing.T) {
	sourceTime := time.Date(2023, 7, 15, 13, 51, 0, 0, time.UTC)
	eventTime := time.Date(2023, 7, 15, 0, 0, 0, 0, time.UTC)
	body := renderStructuredAnswerBody([]recall.Hit{{
		Fact: recall.TemporalFact{
			ID:         "f1",
			Kind:       recall.FactEvent,
			Content:    "On 2023-07-15, Caroline attended a council meeting for adoption.",
			Subject:    "Caroline",
			Predicate:  "attended",
			Object:     "council meeting for adoption",
			ObservedAt: sourceTime,
			ValidFrom:  &eventTime,
			Metadata: map[string]any{
				"valid_from_source": "content_explicit",
				"valid_from_text":   "2023-07-15",
			},
		},
		Evidence: []recall.EvidenceRef{{
			ID:        "D8:9",
			Text:      "Last Friday I went to a council meeting for adoption.",
			Timestamp: sourceTime,
		}},
	}}, "")
	for _, want := range []string{
		`event_time: "2023-07-15"`,
		`event_time_text: "2023-07-15"`,
		`source_time: "2023-07-15 13:51"`,
		`quote: "Last Friday I went to a council meeting for adoption."`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	for _, want := range []string{
		`<answer_candidates>`,
		`event_time_candidate: "2023-07-15"`,
		`event_time_precision: "day"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{
		"EVIDENCE_PACKAGE",
		"relative_time_answer",
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("structured renderer should not emit %q:\n%s", unwanted, body)
		}
	}
}

func TestStructuredAnswerBodyRendersCoarseRelativeTimeAsImpreciseCandidate(t *testing.T) {
	sourceTime := time.Date(2023, 8, 3, 18, 20, 0, 0, time.UTC)
	eventTime := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	hit := recall.Hit{
		Fact: recall.TemporalFact{
			ID:         "f1",
			Kind:       recall.FactEvent,
			Content:    "Maria started volunteering at the homeless shelter about a year ago.",
			Subject:    "Maria",
			ObservedAt: sourceTime,
			ValidFrom:  &eventTime,
			Metadata: map[string]any{
				"valid_from_source":    "content_relative",
				"valid_from_text":      "about a year ago",
				"valid_from_precision": "year",
				"valid_from_timex":     "2022",
			},
		},
		Evidence: []recall.EvidenceRef{{
			ID:        "D27:4",
			Text:      "I started volunteering here about a year ago.",
			Timestamp: sourceTime,
		}},
	}
	body := renderStructuredAnswerBody([]recall.Hit{hit}, "temporal")
	for _, want := range []string{
		`event_time_candidate: "2022"`,
		`event_time: "2022"`,
		`event_time_precision: "year"`,
		`event_time_text: "about a year ago"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `event_time: "2022-01-01"`) || strings.Contains(body, `event_time_candidate: "2022-01-01"`) {
		t.Fatalf("coarse relative time should not render as a precise day:\n%s", body)
	}
	if content := groundedHitContent(hit); !strings.Contains(content, "[time: 2022]") || strings.Contains(content, "[time: 2022-01-01]") {
		t.Fatalf("flattened artifact should preserve year precision, got: %s", content)
	}
	if got := fromRecallArtifact(hit).ValidFrom; got != "2022" {
		t.Fatalf("runner artifact ValidFrom = %q, want 2022", got)
	}
}

func TestStructuredAnswerBodyKeepsRelativeTextWithoutAnswerSideParsing(t *testing.T) {
	sourceTime := time.Date(2023, 7, 15, 13, 51, 0, 0, time.UTC)
	eventTime := time.Date(2023, 7, 15, 0, 0, 0, 0, time.UTC)
	body := renderStructuredAnswerBody([]recall.Hit{{
		Fact: recall.TemporalFact{
			ID:         "f1",
			Kind:       recall.FactEvent,
			Content:    "On 2023-07-15, Alice signed up for the pottery class yesterday.",
			Subject:    "Alice",
			Predicate:  "signed_up",
			Object:     "pottery class",
			ObservedAt: sourceTime,
			ValidFrom:  &eventTime,
			Metadata: map[string]any{
				"valid_from_source": "content_relative",
				"valid_from_text":   "2023-07-15",
			},
		},
		Evidence: []recall.EvidenceRef{{
			ID:        "D8:9",
			Text:      "I signed up for the pottery class yesterday.",
			Timestamp: sourceTime,
		}},
	}}, "")
	for _, want := range []string{
		`event_time: "2023-07-15"`,
		`event_time_text: "2023-07-15"`,
		`quote: "I signed up for the pottery class yesterday."`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `day before`) || strings.Contains(body, `source: "relative_time"`) {
		t.Fatalf("answer context should not parse relative time from prose:\n%s", body)
	}
}

func TestStructuredAnswerBodyRendersAllTemporalFactsWithoutDirectPromotion(t *testing.T) {
	sourceTime := time.Date(2023, 8, 25, 13, 51, 0, 0, time.UTC)
	unrelatedTime := time.Date(2023, 7, 3, 0, 0, 0, 0, time.UTC)
	directTime := time.Date(2023, 8, 24, 0, 0, 0, 0, time.UTC)
	body := renderStructuredAnswerBody([]recall.Hit{
		{
			Fact: recall.TemporalFact{
				ID:        "unrelated",
				Kind:      recall.FactEvent,
				Content:   "On 2023-07-03, Melanie made a bowl.",
				Subject:   "Melanie",
				ValidFrom: &unrelatedTime,
				Metadata: map[string]any{
					"valid_from_source": "content_explicit",
					"valid_from_text":   "2023-07-03",
				},
			},
			Evidence: []recall.EvidenceRef{{Text: "I made a bowl.", Timestamp: unrelatedTime}},
		},
		{
			Fact: recall.TemporalFact{
				ID:        "direct",
				Kind:      recall.FactEvent,
				Content:   "Melanie made a plate in pottery class yesterday.",
				Subject:   "Melanie",
				Object:    "plate",
				ValidFrom: &directTime,
				Metadata: map[string]any{
					"valid_from_source": "content_relative",
					"valid_from_text":   "yesterday",
				},
			},
			Evidence: []recall.EvidenceRef{{
				Text:      "I made a plate in pottery class yesterday.",
				Timestamp: sourceTime,
			}},
		},
	}, "")

	for _, want := range []string{
		`- [#1]`,
		`content: "On 2023-07-03, Melanie made a bowl."`,
		`event_time: "2023-07-03"`,
		`- [#2]`,
		`event_time_candidate: "2023-08-24"`,
		`content: "Melanie made a plate in pottery class yesterday."`,
		`event_time: "2023-08-24"`,
		`event_time_text: "yesterday"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "direct_ranks") {
		t.Fatalf("structured renderer should not emit legacy direct ranks:\n%s", body)
	}
}

func TestStructuredAnswerBodyRendersListFactsWithoutCountCandidate(t *testing.T) {
	body := renderStructuredAnswerBody([]recall.Hit{
		{Fact: recall.TemporalFact{ID: "f1", Kind: recall.FactState, Content: "Melanie has a cat named Bailey.", Subject: "Melanie", Predicate: "has_pet", Object: "Bailey"}, Evidence: []recall.EvidenceRef{{ID: "D1:1", Text: "Melanie has a cat named Bailey."}}},
		{Fact: recall.TemporalFact{ID: "f2", Kind: recall.FactState, Content: "Melanie has a dog named Oliver.", Subject: "Melanie", Predicate: "has_pet", Object: "Oliver"}, Evidence: []recall.EvidenceRef{{ID: "D1:2", Text: "Melanie has a dog named Oliver."}}},
	}, "")
	for _, want := range []string{
		`<answer_candidates>`,
		`content: "Melanie has a cat named Bailey."`,
		`object: "Bailey"`,
		`object_span: "Bailey"`,
		`content: "Melanie has a dog named Oliver."`,
		`object: "Oliver"`,
		`object_span: "Oliver"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{
		"count_answer_candidate",
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("structured renderer should not emit %q:\n%s", unwanted, body)
		}
	}
}

func TestCleanAnswerScalarKeepsFullText(t *testing.T) {
	in := strings.Repeat("猫", 300) + "\n" + strings.Repeat("x", 300)
	got := cleanAnswerScalar(in)
	if strings.Contains(got, "\n") {
		t.Fatalf("cue should normalize newlines: %q", got)
	}
	if !strings.Contains(got, strings.Repeat("猫", 300)) || !strings.Contains(got, strings.Repeat("x", 300)) {
		t.Fatalf("cue should preserve full text: %q", got)
	}
}

func TestStructuredAnswerBodyRendersFullEvidenceForEveryTopKMemory(t *testing.T) {
	body := renderStructuredAnswerBody([]recall.Hit{
		{Fact: recall.TemporalFact{ID: "carried", Kind: recall.FactState, Content: "Avery carried a brass compass.", Subject: "Avery", Object: "brass compass"}, Evidence: []recall.EvidenceRef{{ID: "D1:1", Text: "Avery carried a brass compass."}}},
		{Fact: recall.TemporalFact{ID: "map", Kind: recall.FactState, Content: "Avery likes old maps.", Subject: "Avery"}, Evidence: []recall.EvidenceRef{{ID: "D2:1", Text: "Avery likes old maps."}}},
		{Fact: recall.TemporalFact{ID: "tea", Kind: recall.FactState, Content: "Avery brewed tea.", Subject: "Avery"}, Evidence: []recall.EvidenceRef{{ID: "D3:1", Text: "Avery brewed tea."}}},
		{Fact: recall.TemporalFact{ID: "walk", Kind: recall.FactState, Content: "Avery walked by the river.", Subject: "Avery"}, Evidence: []recall.EvidenceRef{{ID: "D4:1", Text: "Avery walked by the river."}}},
		{Fact: recall.TemporalFact{ID: "book", Kind: recall.FactState, Content: "Avery read a field guide.", Subject: "Avery"}, Evidence: []recall.EvidenceRef{{ID: "D5:1", Text: "Avery read a field guide."}}},
		{Fact: recall.TemporalFact{ID: "bought", Kind: recall.FactEvent, Content: "Avery bought the brass compass in Harbor Market.", Subject: "Avery", Object: "brass compass", Location: "Harbor Market"}, Evidence: []recall.EvidenceRef{{ID: "D1:2", Text: "I bought the brass compass in Harbor Market."}}},
	}, "")

	for _, want := range []string{
		`- [#6]`,
		`evidence_ids: "D1:2"`,
		"evidence:",
		`quote: "I bought the brass compass in Harbor Market."`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
}

func TestStructuredAnswerBodyDoesNotPromoteSourceTimeFallbackToEventTime(t *testing.T) {
	validFrom := time.Date(2024, 5, 7, 9, 30, 0, 0, time.UTC)
	body := renderStructuredAnswerBody([]recall.Hit{{
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
	}}, "")

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
	ctx := structuredAnswerContext(nil, "")
	if ctx.Format != "flowcraftv2_structured_facts" {
		t.Fatalf("unexpected format: %q", ctx.Format)
	}
	for _, want := range []string{
		"structured facts in <retrieved_facts>",
		"untrusted retrieved data",
		"ranked TOPK structured facts",
		"event_time as the event date",
		"observed_at and evidence source_time",
		"content as the canonical extracted fact when it agrees with the evidence",
		"evidence speaker as the speaker of the quoted source turn",
		"Match the form of the question",
		"Start with the shortest supported answer span",
		"concise comma-separated list first",
		"inspect <answer_candidates> first",
		"scan all ranked memories",
		"bounded common knowledge",
		"For WHEN questions, answer from event_time first",
		"HOW MANY, HOW LONG, AGE, duration, count, and comparison",
		"Reply \"I don't know\" only when no memory or evidence quote supports the requested value",
	} {
		if !strings.Contains(ctx.PromptTemplate, want) {
			t.Fatalf("structured prompt missing %q:\n%s", want, ctx.PromptTemplate)
		}
	}
	if strings.Contains(ctx.PromptTemplate, "%s") {
		t.Fatalf("structured prompt should be system-only, got:\n%s", ctx.PromptTemplate)
	}
}

func ptrTime(t time.Time) *time.Time {
	return &t
}

func containsStringValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
