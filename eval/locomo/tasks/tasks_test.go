package tasks

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	"github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/memory/derive"
	derivecontextpack "github.com/GizClaw/flowcraft/memory/derive/context"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	retrievalworkspace "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/model"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestRenderContextUsesRawSourceMessageItems(t *testing.T) {
	pack := &memory.ContextPack{
		Window: recent.WindowResult{Messages: []sourcemessage.Message{
			{
				ID:      "d1",
				Message: model.NewTextMessage(model.RoleUser, "raw transcript evidence"),
				Metadata: map[string]any{
					"dia_id":            "d1",
					"session":           1,
					"session_date_time": "2024-01-01 09:00",
					"speaker":           "Ada",
				},
			},
		}},
		Items: []derive.ContextItem{
			{
				Kind: derive.ContextItemRecentMessage,
				Text: "user: raw transcript evidence",
				Message: &sourcemessage.Message{
					ID:      "d1",
					Seq:     7,
					Message: model.NewTextMessage(model.RoleUser, "raw transcript evidence"),
					Metadata: map[string]any{
						"dia_id":            "d1",
						"session":           1,
						"session_date_time": "2024-01-01 09:00",
						"speaker":           "Ada",
					},
				},
			},
		},
	}

	rendered := renderContext(pack)
	if !strings.Contains(rendered, "raw transcript evidence") {
		t.Fatalf("renderContext() = %q, want raw source-message text", rendered)
	}
	for _, want := range []string{"Recent source messages:", "[R1] dia_id=d1", "session=1", "session_date_time=2024-01-01 09:00", "speaker=Ada", "seq=7", "Text: user: raw transcript evidence"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("renderContext() = %q, want provenance %q", rendered, want)
		}
	}
}

func TestRenderContextGroupsRetrievedBeforeRecentAndDedupes(t *testing.T) {
	pack := &memory.ContextPack{Items: []derive.ContextItem{
		{
			Kind: derive.ContextItemRecentMessage,
			Text: "duplicate recent should not render",
			Message: &sourcemessage.Message{
				ID:       "d1",
				Seq:      1,
				Metadata: map[string]any{"dia_id": "d1", "session": 1, "speaker": "Ada"},
			},
		},
		{
			Kind: derive.ContextItemRecentMessage,
			Text: "retrieved transcript evidence",
			Retrieval: &retrieval.Hit{Doc: retrieval.Doc{
				ID: "doc-d1",
				Metadata: map[string]any{
					"dia_id":           "d1",
					"session":          2,
					"session_datetime": "2024-01-02 09:00",
					"speaker":          "Ada",
					"seq":              9,
				},
			}, Score: 0.99},
		},
		{
			Kind: derive.ContextItemRecentMessage,
			Text: "recent transcript context",
			Message: &sourcemessage.Message{
				ID:       "d2",
				Seq:      10,
				Metadata: map[string]any{"dia_id": "d2", "session": 2, "speaker": "Ben"},
			},
		},
	}}

	rendered := renderContext(pack)
	retrievedPos := strings.Index(rendered, "Retrieved source messages:")
	recentPos := strings.Index(rendered, "Recent source messages:")
	if retrievedPos < 0 || recentPos < 0 || retrievedPos > recentPos {
		t.Fatalf("renderContext() sections out of order:\n%s", rendered)
	}
	for _, want := range []string{
		"[1] dia_id=d1 | session=2 | session_datetime=2024-01-02 09:00 | speaker=Ada | seq=9",
		"Text: retrieved transcript evidence",
		"[R1] dia_id=d2 | session=2 | speaker=Ben | seq=10",
		"Text: recent transcript context",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("renderContext() missing %q:\n%s", want, rendered)
		}
	}
	for _, unwanted := range []string{"duplicate recent should not render", "score=", "doc_id=doc-d1"} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("renderContext() contains noisy or duplicate text %q:\n%s", unwanted, rendered)
		}
	}
}

func TestRenderContextDoesNotRenderEntityFactItems(t *testing.T) {
	pack := &memory.ContextPack{Items: []derive.ContextItem{
		{
			Kind: derive.ContextItemEntityFact,
			Text: "fact text should stay out of the answer prompt",
			EntityFact: &viewentityfact.Fact{
				ID:       "fact-1",
				FactText: "fact text should stay out of the answer prompt",
			},
			Retrieval: &retrieval.Hit{Doc: retrieval.Doc{ID: "fact-doc"}, Score: 0.9},
		},
		{
			Kind: derive.ContextItemRecentMessage,
			Text: "user: source transcript should render",
			Message: &sourcemessage.Message{
				ID:       "d1",
				Metadata: map[string]any{"dia_id": "d1"},
			},
			Retrieval: &retrieval.Hit{Doc: retrieval.Doc{ID: "source-doc"}, Score: 0.8},
		},
	}}

	rendered := renderContext(pack)
	if strings.Contains(rendered, "fact text should stay out of the answer prompt") {
		t.Fatalf("renderContext rendered entity fact text:\n%s", rendered)
	}
	if !strings.Contains(rendered, "source transcript should render") {
		t.Fatalf("renderContext missing source message:\n%s", rendered)
	}
}

func TestContextHitCountsReportsSourceMessages(t *testing.T) {
	pack := &memory.ContextPack{Items: []derive.ContextItem{
		{Kind: derive.ContextItemRecentMessage, Text: "Ada likes tea", Message: &sourcemessage.Message{ID: "d1"}},
		{Kind: derive.ContextItemRecentMessage, Text: "Ben mentioned coffee", Message: &sourcemessage.Message{ID: "d2"}},
	}}

	counts := contextHitCounts(pack)
	if counts.SourceMessages != 2 || counts.SummaryNode != 0 || counts.DocumentChunk != 0 {
		t.Fatalf("hit counts = %+v, want source-message baseline counts", counts)
	}
}

func TestDefaultQATopKIsTwelve(t *testing.T) {
	if DefaultQATopK != 12 {
		t.Fatalf("DefaultQATopK = %d, want 12", DefaultQATopK)
	}
}

func TestNormalizeEvidenceIDsSplitsCompositeEvidence(t *testing.T) {
	got := NormalizeEvidenceIDs([]string{"D8:6; D9:17", " D9:17 ", "D10:1|D10:2"})
	want := []string{"D8:6", "D9:17", "D10:1", "D10:2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("NormalizeEvidenceIDs = %v, want %v", got, want)
	}
}

func TestEvidenceRecallUsesAtomicEvidenceIDs(t *testing.T) {
	observed := map[string]bool{"D8:6": true, "D10:2": true}
	got := evidenceRecall(observed, []string{"D8:6; D9:17", "D10:1|D10:2"})
	want := 0.5
	if got != want {
		t.Fatalf("evidenceRecall = %.3f, want %.3f", got, want)
	}
}

func TestRetrieveQAContextDisablesRecentWindow(t *testing.T) {
	for _, tc := range []struct {
		name            string
		qaTopK          int
		wantMessageHits int
	}{
		{name: "default top k", qaTopK: 0, wantMessageHits: DefaultQATopK + qaSummaryExpandedMaxSource + qaEntityExpandedMaxSource},
		{name: "custom top k", qaTopK: 7, wantMessageHits: 7 + qaSummaryExpandedMaxSource + qaEntityExpandedMaxSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := sdkworkspace.NewMemWorkspace()
			index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
			if err != nil {
				t.Fatalf("retrieval workspace: %v", err)
			}
			defer func() { _ = index.Close() }()

			mem, err := memory.New(memory.Spec{
				Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
				Capabilities: []memory.CapabilitySpec{
					{Capability: memory.CapabilityRecentWindow, Required: true},
					{Capability: memory.CapabilityMessageIndex, Required: true},
				},
				Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
				WriteStages: []memory.StageSpec{
					{Name: "append_message"},
					{Name: "index_messages"},
				},
				ReadStages: []memory.StageSpec{
					{Name: "load_recent_messages"},
					{Name: "retrieve_messages"},
					{Name: "pack_context"},
				},
			}, memory.Deps{
				MessageStore: sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message")),
				Index:        index,
			})
			if err != nil {
				t.Fatalf("memory.New: %v", err)
			}

			scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv", DatasetID: "dataset"}
			messages := make([]sourcemessage.Message, 40)
			for i := range messages {
				n := i + 1
				messages[i] = sourcemessage.Message{
					ID:      "dia-" + strconv.Itoa(n),
					Message: model.NewTextMessage(model.RoleUser, "needle QA evidence message "+strconv.Itoa(n)),
				}
			}
			if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{Scope: scope, Messages: messages}); err != nil {
				t.Fatalf("AppendMessage: %v", err)
			}

			pack, err := RetrieveQAContextForDiagnostics(ctx, mem, scope, dataset.QAItem{Question: "needle QA evidence"}, tc.qaTopK)
			if err != nil {
				t.Fatalf("retrieveQAContext: %v", err)
			}
			if got := len(pack.MessageHits); got != tc.wantMessageHits {
				t.Fatalf("message hits = %d, want %d", got, tc.wantMessageHits)
			}
			if got := len(pack.Window.Messages); got != 0 {
				t.Fatalf("recent window messages = %d, want 0", got)
			}
		})
	}
}

func TestRetrieveQAContextDefaultGraphBudgetDoesNotExpandGraphSources(t *testing.T) {
	ctx := context.Background()
	mem, entityStore, closeMem := newQAGraphOnlyMemory(t)
	defer closeMem()
	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	messages := []sourcemessage.Message{
		{ID: "dia-1", Message: model.NewTextMessage(model.RoleUser, "irrelevant filler")},
		{ID: "dia-2", Message: model.NewTextMessage(model.RoleUser, "Ada keeps the spare key in the blue planter.")},
	}
	if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{Scope: scope, Messages: messages}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	putQAGraphEntity(t, ctx, entityStore, scope, viewentityfact.Entity{ID: "ent_ada", Type: viewentityfact.EntityPerson, Name: "Ada"})
	putQAGraphFact(t, ctx, entityStore, scope, viewentityfact.Fact{
		ID:              "fact_key",
		SubjectEntityID: "ent_ada",
		RelationType:    viewentityfact.RelationPossession,
		FactText:        "Ada keeps the spare key in the blue planter.",
		SourceRefs:      []views.SourceRef{qaTestMessageRef("conv", "dia-2")},
	})

	pack, err := RetrieveQAContextForDiagnostics(ctx, mem, scope, dataset.QAItem{Question: "Where does Ada keep the spare key?"}, DefaultQATopK)
	if err != nil {
		t.Fatalf("RetrieveQAContextForDiagnostics: %v", err)
	}
	counts := contextHitCounts(pack)
	if counts.SourceGraphExpanded != 0 || counts.GraphFact != 0 {
		t.Fatalf("hit counts = %+v, want graph disabled by default", counts)
	}
	if rendered := renderContext(pack); strings.Contains(rendered, "blue planter") {
		t.Fatalf("rendered context included graph evidence with default graph budget:\n%s", rendered)
	}
}

func TestRetrieveQAContextHydratesSummaryExpandedSourceRefs(t *testing.T) {
	ctx := context.Background()
	mem, closeMem := newQASummaryOnlyMemory(t, qaTestSummarizer{
		nodes: [][]int{{0}},
		texts: []string{"bridge-token summary should not be rendered as evidence"},
	})
	defer closeMem()

	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	messages := make([]sourcemessage.Message, 6)
	messages[0] = sourcemessage.Message{
		ID:      "dia-1",
		Message: model.NewTextMessage(model.RoleUser, "Ada hid the spare key under the blue planter."),
		Metadata: map[string]any{
			"dia_id":  "dia-1",
			"session": 1,
		},
	}
	for i := 1; i < len(messages); i++ {
		messages[i] = sourcemessage.Message{
			ID:      fmt.Sprintf("dia-%d", i+1),
			Message: model.NewTextMessage(model.RoleUser, fmt.Sprintf("recent filler %d", i+1)),
		}
	}
	if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{Scope: scope, Messages: messages}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	pack, err := RetrieveQAContextForDiagnostics(ctx, mem, scope, dataset.QAItem{Question: "bridge-token"}, 5)
	if err != nil {
		t.Fatalf("retrieveQAContext: %v", err)
	}
	rendered := renderContext(pack)
	if !strings.Contains(rendered, "Ada hid the spare key under the blue planter.") {
		t.Fatalf("rendered context missing hydrated source message:\n%s", rendered)
	}
	if strings.Contains(rendered, "bridge-token summary should not be rendered as evidence") {
		t.Fatalf("rendered context includes summary text as evidence:\n%s", rendered)
	}
	counts := contextHitCounts(pack)
	if counts.SourceSummaryExpanded != 1 || counts.SummaryNode != 1 {
		t.Fatalf("hit counts = %+v, want one summary-expanded source and one summary hit", counts)
	}
}

func TestRetrieveQAContextDedupesDirectBehindSummaryExpansion(t *testing.T) {
	ctx := context.Background()
	mem, closeMem := newQASummaryMemory(t, qaTestSummarizer{
		nodes: [][]int{{0}},
		texts: []string{"shared-needle summary"},
	})
	defer closeMem()

	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	messages := []sourcemessage.Message{{
		ID:      "dia-1",
		Message: model.NewTextMessage(model.RoleUser, "shared-needle direct transcript evidence"),
		Metadata: map[string]any{
			"dia_id": "dia-1",
		},
	}}
	for i := 2; i <= 6; i++ {
		messages = append(messages, sourcemessage.Message{
			ID:      fmt.Sprintf("dia-%d", i),
			Message: model.NewTextMessage(model.RoleUser, fmt.Sprintf("recent filler %d", i)),
		})
	}
	if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{Scope: scope, Messages: messages}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	pack, err := RetrieveQAContextForDiagnostics(ctx, mem, scope, dataset.QAItem{Question: "shared-needle"}, 5)
	if err != nil {
		t.Fatalf("retrieveQAContext: %v", err)
	}
	counts := contextHitCounts(pack)
	if counts.SourceDirect != 5 || counts.SourceSummaryExpanded != 1 {
		t.Fatalf("hit counts = %+v, want summary-expanded evidence plus direct budget without recent", counts)
	}
	if got := countContextMessagesByID(pack, "dia-1"); got != 1 {
		t.Fatalf("message dia-1 appears %d times, want 1", got)
	}
}

func TestRetrieveQAContextLimitsSummaryExpansionPerNodeAndTotal(t *testing.T) {
	ctx := context.Background()
	mem, closeMem := newQASummaryOnlyMemory(t, qaTestSummarizer{
		nodes: [][]int{
			{0, 1, 2, 3},
			{4, 5, 6, 7},
			{8, 9, 10, 11},
			{12, 13, 14, 15},
			{16, 17, 18, 19},
			{20, 21, 22, 23},
		},
		texts: []string{
			"budget-bridge first summary",
			"budget-bridge second summary",
			"budget-bridge third summary",
			"budget-bridge fourth summary",
			"budget-bridge fifth summary",
			"budget-bridge sixth summary",
		},
	})
	defer closeMem()

	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	messages := make([]sourcemessage.Message, 30)
	for i := range messages {
		messages[i] = sourcemessage.Message{
			ID:      fmt.Sprintf("dia-%02d", i+1),
			Message: model.NewTextMessage(model.RoleUser, fmt.Sprintf("source evidence %02d", i+1)),
		}
	}
	if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{Scope: scope, Messages: messages}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	pack, err := RetrieveQAContextForDiagnostics(ctx, mem, scope, dataset.QAItem{Question: "budget-bridge"}, qaSummaryExpandedMaxSource)
	if err != nil {
		t.Fatalf("retrieveQAContext: %v", err)
	}
	counts := contextHitCounts(pack)
	if counts.SourceSummaryExpanded != qaSummaryExpandedMaxSource {
		t.Fatalf("summary-expanded count = %d, want %d", counts.SourceSummaryExpanded, qaSummaryExpandedMaxSource)
	}
	if counts.SourceNeighborhoodExpanded == 0 {
		t.Fatalf("source-neighborhood count = 0, want capped summary neighbors appended separately")
	}
	for _, id := range []string{"dia-04", "dia-08", "dia-12", "dia-16", "dia-20", "dia-24"} {
		if got := countContextMessagesByIDAndOrigin(pack, id, qaRetrievalOriginSummary); got != 0 {
			t.Fatalf("message %s appears %d times as summary-expanded, want 0 from capped expansion", id, got)
		}
	}
}

func TestRetrieveQAContextUsesIndependentRecentSummaryAndDirectBudgets(t *testing.T) {
	ctx := context.Background()
	mem, closeMem := newQASummaryMemory(t, qaTestSummarizer{
		nodes: [][]int{
			{0, 1, 2},
			{3, 4, 5},
			{6, 7, 8},
			{9, 10, 11},
			{12, 13, 14},
			{15, 16, 17},
		},
		texts: []string{
			"budget mix summary first",
			"budget mix summary second",
			"budget mix summary third",
			"budget mix summary fourth",
			"budget mix summary fifth",
			"budget mix summary sixth",
		},
	})
	defer closeMem()

	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	messages := make([]sourcemessage.Message, 50)
	for i := range messages {
		messages[i] = sourcemessage.Message{
			ID:      fmt.Sprintf("dia-%02d", i+1),
			Message: model.NewTextMessage(model.RoleUser, fmt.Sprintf("budget mix direct source %02d", i+1)),
		}
	}
	if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{Scope: scope, Messages: messages}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	pack, err := RetrieveQAContextForDiagnostics(ctx, mem, scope, dataset.QAItem{Question: "budget mix"}, DefaultQATopK)
	if err != nil {
		t.Fatalf("retrieveQAContext: %v", err)
	}
	counts := contextHitCounts(pack)
	if counts.SourceSummaryExpanded != qaSummaryExpandedMaxSource {
		t.Fatalf("summary-expanded count = %d, want %d", counts.SourceSummaryExpanded, qaSummaryExpandedMaxSource)
	}
	if counts.SourceDirect != DefaultQATopK {
		t.Fatalf("source direct count = %d, want %d", counts.SourceDirect, DefaultQATopK)
	}
	if counts.SourceMessages != qaSummaryExpandedMaxSource+DefaultQATopK {
		t.Fatalf("source message count = %d, want summary+direct budget", counts.SourceMessages)
	}
	for _, id := range []string{"dia-01", "dia-18"} {
		if got := countContextMessagesByID(pack, id); got != 1 {
			t.Fatalf("summary message %s appears %d times, want exactly once", id, got)
		}
	}
	for _, id := range []string{"dia-46", "dia-50"} {
		if got := countContextMessagesByID(pack, id); got != 0 {
			t.Fatalf("recent message %s appears %d times, want 0 with recent disabled", id, got)
		}
	}
}

func TestRetrieveQAContextUsesDrilledDownSummaryHits(t *testing.T) {
	ctx := context.Background()
	mem, closeMem := newQASummaryOnlyMemory(t, layeredQATestSummarizer{})
	defer closeMem()

	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	messages := []sourcemessage.Message{
		{ID: "dia-1", Message: model.NewTextMessage(model.RoleUser, "wrong source one")},
		{ID: "dia-2", Message: model.NewTextMessage(model.RoleUser, "wrong source two")},
		{ID: "dia-3", Message: model.NewTextMessage(model.RoleUser, "wrong source three")},
		{ID: "dia-4", Message: model.NewTextMessage(model.RoleUser, "Ada left the umbrella beside the piano.")},
	}
	for i := 5; i <= 9; i++ {
		messages = append(messages, sourcemessage.Message{
			ID:      fmt.Sprintf("dia-%d", i),
			Message: model.NewTextMessage(model.RoleUser, fmt.Sprintf("recent filler %d", i)),
		})
	}
	if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{Scope: scope, Messages: messages}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	pack, err := RetrieveQAContextForDiagnostics(ctx, mem, scope, dataset.QAItem{Question: "umbrella bridge"}, 1)
	if err != nil {
		t.Fatalf("retrieveQAContext: %v", err)
	}
	if len(pack.SummaryHits) == 0 || pack.SummaryHits[0].Node.ID != "leaf-4" {
		t.Fatalf("SummaryHits = %+v, want drilled-down relevant leaf", pack.SummaryHits)
	}
	rendered := renderContext(pack)
	if !strings.Contains(rendered, "Ada left the umbrella beside the piano.") {
		t.Fatalf("rendered context missing drilled-down child source:\n%s", rendered)
	}
}

func TestRetrieveQAContextCanDisableSummaryDrillDownFromSpec(t *testing.T) {
	ctx := context.Background()
	mem, closeMem := newQASummaryOnlyMemoryWithConfig(t, layeredQATestSummarizer{}, map[string]any{
		"drilldown_max_depth": 0,
	})
	defer closeMem()

	scope := memory.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	messages := []sourcemessage.Message{
		{ID: "dia-1", Message: model.NewTextMessage(model.RoleUser, "wrong source one")},
		{ID: "dia-2", Message: model.NewTextMessage(model.RoleUser, "wrong source two")},
		{ID: "dia-3", Message: model.NewTextMessage(model.RoleUser, "wrong source three")},
		{ID: "dia-4", Message: model.NewTextMessage(model.RoleUser, "Ada left the umbrella beside the piano.")},
	}
	for i := 5; i <= 9; i++ {
		messages = append(messages, sourcemessage.Message{
			ID:      fmt.Sprintf("dia-%d", i),
			Message: model.NewTextMessage(model.RoleUser, fmt.Sprintf("recent filler %d", i)),
		})
	}
	if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{Scope: scope, Messages: messages}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	pack, err := RetrieveQAContextForDiagnostics(ctx, mem, scope, dataset.QAItem{Question: "umbrella bridge"}, 1)
	if err != nil {
		t.Fatalf("retrieveQAContext: %v", err)
	}
	if len(pack.SummaryHits) == 0 || pack.SummaryHits[0].Node.ID != "parent-1" {
		t.Fatalf("SummaryHits = %+v, want original parent hit when drill-down is disabled", pack.SummaryHits)
	}
	rendered := renderContext(pack)
	if !strings.Contains(rendered, "wrong source one") {
		t.Fatalf("rendered context = %s, want parent SourceRefs without drill-down", rendered)
	}
}

func TestObservedDiaIDsUsesRawSourceMessageItems(t *testing.T) {
	pack := &memory.ContextPack{
		Items: []derive.ContextItem{{
			Kind: derive.ContextItemRecentMessage,
			Text: "raw evidence",
			Message: &sourcemessage.Message{
				ID:       "d1",
				Metadata: map[string]any{"dia_id": "d1"},
			},
		}},
	}
	if got := evidenceRecall(observedDiaIDs(pack), []string{"d1"}); got != 1 {
		t.Fatalf("raw source-message evidence recall = %.3f, want 1", got)
	}
}

func TestQAAnswerPromptUsesBestEffortGuidance(t *testing.T) {
	item := dataset.QAItem{Question: "What does Ada like?", Answer: "tea"}
	pack := &memory.ContextPack{Items: []derive.ContextItem{{
		Kind: derive.ContextItemRecentMessage,
		Text: "Ada likes tea.\nImage caption: a red mug on a table",
		Message: &sourcemessage.Message{
			ID:       "d1",
			Metadata: map[string]any{"dia_id": "d1"},
		},
	}}}
	messages := qaAnswerMessages(item, pack)
	if len(messages) != 2 {
		t.Fatalf("qaAnswerMessages len = %d, want 2", len(messages))
	}
	systemPrompt := messages[0].Content()
	userPrompt := messages[1].Content()
	for _, want := range []string{
		"Context format:",
		"`Retrieved source messages` are the primary candidate support",
		"`Recent source messages` are supplemental recent context",
		"Content-bearing metadata can also be evidence",
		"`image_caption`",
		"`caption`",
		"`blip_caption`",
		"`query`",
		"`session_date_time` / `session_datetime` anchor relative time expressions",
		"`speaker` is the real speaker",
		"`dia_id` and `seq` only distinguish messages",
		"Use a single policy for all questions",
		"Answer only from information supported by the source-message context",
		"First identify every constraint",
		"subject/person, relationship, event/object, time",
		"Related keywords alone are not enough",
		"same subject, relationship, or event",
		"do not transfer similar facts",
		"Do not require exact phrase overlap",
		"same subject and constraints are supported by message text or content-bearing metadata",
		"Use this answerability order",
		"direct support, compatible multi-message support, reasonable same-subject inference, supported partial answer, then refusal",
		"give the shortest complete answer",
		"reasonable inference",
		"likely/would/probably/preference questions",
		"supported tendency instead of refusing",
		"scan all source messages about the same subject/event",
		"Collect every supported required part",
		"deduplicate equivalent parts",
		"do not stop after the first matching message",
		"supported partial answer",
		"Do not refuse just because one requested part is missing",
		`For yes/no questions, answer "No" only when`,
		"showing that a different subject owns the item or performed the action",
		`Do not answer "No" merely because the context lacks support for "Yes"`,
		"bind relative references to the source message's session date/time",
		"Convert to an absolute date, month, or year when the conversion is clear",
		"Preserve the source anchored expression",
		"Do not refuse because the support gives only a year or month",
		`Say exactly "No information available." only as the final fallback`,
		"fails the question's subject, relationship, event, and time constraints",
		"Return only answer text",
		"Fictional mini-examples:",
		"7 May 2023",
		"the Friday before 15 July 2023",
		"the Sunday before 25 May 2023",
		"sunscreen and bus tickets",
		"beach, mountains, and forest",
		"the blue notebook",
		"likely yes",
		"Community Garden Day",
		"Portland",
		"Omar's grandmother",
		"Did Rina make the clay vase?",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Fatalf("answer system prompt missing %q:\n%s", want, systemPrompt)
		}
	}
	for _, leaked := range []string{
		"What does Ada like?",
		"Ada likes tea",
		"Image caption: a red mug on a table",
	} {
		if strings.Contains(systemPrompt, leaked) {
			t.Fatalf("answer system prompt contains user payload %q:\n%s", leaked, systemPrompt)
		}
	}
	lowerSystemPrompt := strings.ToLower(systemPrompt)
	for _, banned := range []string{"locomo", "category", "judge", "gold", "evidence_recall", "score"} {
		if strings.Contains(lowerSystemPrompt, banned) {
			t.Fatalf("answer system prompt contains benchmark term %q:\n%s", banned, systemPrompt)
		}
	}
	if !strings.Contains(userPrompt, "Source-message context:") || !strings.Contains(userPrompt, "Image caption: a red mug on a table") {
		t.Fatalf("answer user prompt missing raw source-message context:\n%s", userPrompt)
	}
	if !strings.Contains(userPrompt, "Question:\nWhat does Ada like?") {
		t.Fatalf("answer user prompt missing question:\n%s", userPrompt)
	}
}

func TestParseQAJudgeResultBinarizesPartialAndRejectsInvalidJSON(t *testing.T) {
	got, err := parseQAJudgeResult(`{"verdict":"partially correct","rationale":"close"}`, dataset.QAItem{Category: "single-hop"})
	if err != nil {
		t.Fatalf("parse legacy partial judge result error = %v", err)
	}
	if got.Verdict != "incorrect" || got.Correct {
		t.Fatalf("legacy partial judge result = %+v, want incorrect", got)
	}
	if _, err := parseQAJudgeResult(`not json`, dataset.QAItem{}); err == nil {
		t.Fatal("parse invalid judge JSON error = nil")
	}
}

func TestParseQAJudgeResultBinarizesLegacyNoInfoCorrect(t *testing.T) {
	answerable, err := parseQAJudgeResult(`{"verdict":"no_info_correct"}`, dataset.QAItem{Category: "temporal", Answer: "2023"})
	if err != nil {
		t.Fatalf("parse answerable no-info judge result error = %v", err)
	}
	if answerable.Verdict != "incorrect" || answerable.Correct {
		t.Fatalf("answerable no-info judge result = %+v, want forced incorrect", answerable)
	}
	adversarial, err := parseQAJudgeResult(`{"verdict":"no_info_correct"}`, dataset.QAItem{Category: "adversarial", CategoryID: 5})
	if err != nil {
		t.Fatalf("parse adversarial no-info judge result error = %v", err)
	}
	if adversarial.Verdict != "correct" || !adversarial.Correct {
		t.Fatalf("adversarial no-info judge result = %+v, want binary correct", adversarial)
	}
}

func TestQAJudgeMessagesSeparatePolicyFromPayload(t *testing.T) {
	item := dataset.QAItem{Question: "What does Ada like?", Answer: "tea", Category: "single-hop", CategoryID: 4}
	messages := qaJudgeMessages(item, "tea")
	if len(messages) != 2 {
		t.Fatalf("qaJudgeMessages len = %d, want 2", len(messages))
	}
	systemPrompt := messages[0].Content()
	userPrompt := messages[1].Content()
	for _, leaked := range []string{item.Question, item.Answer, "Predicted answer"} {
		if strings.Contains(systemPrompt, leaked) {
			t.Fatalf("judge system prompt contains payload %q:\n%s", leaked, systemPrompt)
		}
	}
	for _, want := range []string{
		`"verdict": "correct|incorrect"`,
		"Grading rules:",
		"more specific than the gold answer as correct if the extra specificity does not contradict",
		"accept equivalent granularity or a more specific date inside the same stated time range",
		"explicitly refuses, says no information, or says there is not enough information",
		"do not mark an answer incorrect only because it includes a non-conflicting detail",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Fatalf("judge system prompt missing %q:\n%s", want, systemPrompt)
		}
	}
	for _, want := range []string{"Question: What does Ada like?", "Gold answer: tea", "Predicted answer: tea"} {
		if !strings.Contains(userPrompt, want) {
			t.Fatalf("judge user prompt missing %q:\n%s", want, userPrompt)
		}
	}
}

func TestScoreQAAdversarialRequiresNoInfoAnswer(t *testing.T) {
	item := dataset.QAItem{
		Question:          "Where did Ada hide the keys?",
		CategoryID:        5,
		Category:          "adversarial",
		AdversarialAnswer: "under the red mug",
	}
	if got := scoreQA(item, "under the red mug"); got != 0 {
		t.Fatalf("scoreQA adversarial lure = %.3f, want 0", got)
	}
	if got := scoreQA(item, "No information available."); got != 1 {
		t.Fatalf("scoreQA adversarial no-info = %.3f, want 1", got)
	}
}

type qaTestSummarizer struct {
	nodes [][]int
	texts []string
}

func (s qaTestSummarizer) Summarize(_ context.Context, input derive.SummaryInput) ([]recent.SummaryNode, error) {
	nodes := make([]recent.SummaryNode, 0, len(s.nodes))
	for nodeIndex, refIndexes := range s.nodes {
		refs := make([]views.SourceRef, 0, len(refIndexes))
		revisions := make([]views.SourceRevision, 0, len(refIndexes))
		for _, refIndex := range refIndexes {
			if refIndex < 0 || refIndex >= len(input.Window.SourceRefs) || refIndex >= len(input.Window.Messages) {
				continue
			}
			ref := input.Window.SourceRefs[refIndex]
			sourceKey, err := ref.StableKeyE()
			if err != nil {
				return nil, err
			}
			msg := input.Window.Messages[refIndex]
			refs = append(refs, ref)
			revisions = append(revisions, views.SourceRevision{
				Kind:        views.SourceMessage,
				SourceKey:   sourceKey,
				Revision:    strconv.FormatUint(msg.Seq, 10),
				ContentHash: "test-content-hash",
				ObservedAt:  time.Unix(int64(refIndex+1), 0),
			})
		}
		if len(refs) == 0 {
			continue
		}
		summaryText := fmt.Sprintf("test summary %d", nodeIndex+1)
		if nodeIndex < len(s.texts) {
			summaryText = s.texts[nodeIndex]
		}
		now := time.Unix(int64(nodeIndex+1), 0)
		nodes = append(nodes, recent.SummaryNode{
			ID:         recent.NodeID(fmt.Sprintf("summary-%02d", nodeIndex+1)),
			Scope:      input.Scope,
			SourceRefs: refs,
			Summary:    summaryText,
			Level:      0,
			Signature: views.ViewSignature{
				ViewID:             input.View.ID,
				SourceRevisions:    revisions,
				TransformSignature: "qa-test-summarizer",
			},
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	return nodes, nil
}

type layeredQATestSummarizer struct{}

func (layeredQATestSummarizer) Summarize(_ context.Context, input derive.SummaryInput) ([]recent.SummaryNode, error) {
	leaf := func(id recent.NodeID, refIndex int, text string) (recent.SummaryNode, error) {
		if refIndex < 0 || refIndex >= len(input.Window.SourceRefs) || refIndex >= len(input.Window.Messages) {
			return recent.SummaryNode{}, fmt.Errorf("ref index %d out of range", refIndex)
		}
		ref := input.Window.SourceRefs[refIndex]
		sourceKey, err := ref.StableKeyE()
		if err != nil {
			return recent.SummaryNode{}, err
		}
		msg := input.Window.Messages[refIndex]
		now := time.Unix(int64(refIndex+1), 0)
		return recent.SummaryNode{
			ID:         id,
			Scope:      input.Scope,
			SourceRefs: []views.SourceRef{ref},
			Summary:    text,
			Level:      0,
			Signature: views.ViewSignature{
				ViewID: input.View.ID,
				SourceRevisions: []views.SourceRevision{{
					Kind:        views.SourceMessage,
					SourceKey:   sourceKey,
					Revision:    strconv.FormatUint(msg.Seq, 10),
					ContentHash: "test-content-hash",
					ObservedAt:  now,
				}},
				TransformSignature: "qa-layered-test-summarizer",
			},
			CreatedAt: now,
			UpdatedAt: now,
		}, nil
	}

	leaves := make([]recent.SummaryNode, 0, 4)
	for i, spec := range []struct {
		id   recent.NodeID
		text string
	}{
		{"leaf-1", "irrelevant kitchen note"},
		{"leaf-2", "irrelevant travel note"},
		{"leaf-3", "irrelevant music note"},
		{"leaf-4", "umbrella detail child evidence"},
	} {
		node, err := leaf(spec.id, i, spec.text)
		if err != nil {
			return nil, err
		}
		leaves = append(leaves, node)
	}

	var refs []views.SourceRef
	var revisions []views.SourceRevision
	var parentIDs []recent.NodeID
	for _, node := range leaves {
		refs = append(refs, node.SourceRefs...)
		revisions = append(revisions, node.Signature.SourceRevisions...)
		parentIDs = append(parentIDs, node.ID)
	}
	parent := recent.SummaryNode{
		ID:         "parent-1",
		Scope:      input.Scope,
		ParentIDs:  parentIDs,
		SourceRefs: refs,
		Summary:    "umbrella bridge umbrella bridge parent overview",
		Level:      1,
		Signature: views.ViewSignature{
			ViewID:             input.View.ID,
			SourceRevisions:    revisions,
			TransformSignature: "qa-layered-test-summarizer",
		},
		CreatedAt: time.Unix(10, 0),
		UpdatedAt: time.Unix(10, 0),
	}
	return append(leaves, parent), nil
}

func newQASummaryMemory(t *testing.T, summarizer derive.Summarizer) (*memory.System, func()) {
	return newQAMemoryWithSummary(t, summarizer, true)
}

func newQASummaryOnlyMemory(t *testing.T, summarizer derive.Summarizer) (*memory.System, func()) {
	return newQAMemoryWithSummary(t, summarizer, false)
}

func newQASummaryOnlyMemoryWithConfig(t *testing.T, summarizer derive.Summarizer, summaryStageConfig map[string]any) (*memory.System, func()) {
	return newQAMemoryWithSummaryConfig(t, summarizer, false, summaryStageConfig)
}

func newQAMemoryWithSummary(t *testing.T, summarizer derive.Summarizer, retrieveMessages bool) (*memory.System, func()) {
	return newQAMemoryWithSummaryConfig(t, summarizer, retrieveMessages, nil)
}

func newQAMemoryWithSummaryConfig(t *testing.T, summarizer derive.Summarizer, retrieveMessages bool, summaryStageConfig map[string]any) (*memory.System, func()) {
	t.Helper()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	readStages := []memory.StageSpec{
		{Name: "load_recent_messages"},
	}
	if retrieveMessages {
		readStages = append(readStages, memory.StageSpec{Name: "retrieve_messages"})
	}
	readStages = append(
		readStages,
		memory.StageSpec{Name: "retrieve_summaries", Config: summaryStageConfig},
		memory.StageSpec{Name: "pack_context"},
	)
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityMessageIndex, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityMessageIndex, Namespace: "message_index", Required: true},
			{Capability: memory.CapabilitySummaryDAG, Namespace: "summary_dag", Required: true},
		},
		WriteStages: []memory.StageSpec{
			{Name: "append_message"},
			{Name: "index_messages"},
			{Name: "build_summary_dag"},
		},
		ReadStages: readStages,
	}, memory.Deps{
		MessageStore:  sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message")),
		SummaryStore:  recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag")),
		Summarizer:    summarizer,
		Index:         index,
		ContextPacker: qaSourceEvidenceTestPacker(),
	})
	if err != nil {
		_ = index.Close()
		t.Fatalf("memory.New: %v", err)
	}
	cleanup := func() {
		if err := mem.Close(); err != nil {
			t.Fatalf("memory close: %v", err)
		}
		if err := index.Close(); err != nil {
			t.Fatalf("index close: %v", err)
		}
	}
	return mem, cleanup
}

func newQAGraphOnlyMemory(t *testing.T) (*memory.System, *viewentityfact.WorkspaceStore, func()) {
	t.Helper()
	root := sdkworkspace.NewMemWorkspace()
	entityStore := viewentityfact.NewWorkspaceStore(sdkworkspace.Sub(root, "views/entity_facts"))
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
		},
		WriteStages: []memory.StageSpec{
			{Name: "append_message"},
		},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "pack_context"},
		},
	}, memory.Deps{
		MessageStore:    sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message")),
		EntityFactStore: entityStore,
		ContextPacker:   qaSourceEvidenceTestPacker(),
	})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	cleanup := func() {
		if err := mem.Close(); err != nil {
			t.Fatalf("memory close: %v", err)
		}
	}
	return mem, entityStore, cleanup
}

func qaSourceEvidenceTestPacker() derivecontextpack.SourceEvidencePacker {
	return derivecontextpack.SourceEvidencePacker{
		SourceOnly:              true,
		MaxDirectMessages:       DefaultQATopK,
		MaxSummaryMessages:      qaSummaryExpandedMaxSource,
		MaxEntityFactMessages:   qaEntityExpandedMaxSource,
		MaxGraphMessages:        DefaultQAGraphExpandedMaxSource,
		MaxNeighborhoodMessages: qaSummaryNeighborMaxSource,
		MaxSourceRefsPerHit:     qaSummaryRefsPerNode,
		MinEntityConfidence:     qaEntitySupplementMinConfidence,
		MinRelativeScore:        qaEntitySupplementMinRelative,
		UseDirectMessages:       true,
		UseSummaryRefs:          true,
		UseEntityFactRefs:       true,
		UseGraphSources:         true,
		UseNeighborhood:         true,
		NeighborhoodBefore:      qaSummaryNeighborBefore,
		NeighborhoodAfter:       qaSummaryNeighborAfter,
		NeighborhoodAnchors:     []derivecontextpack.SourceEvidenceOrigin{derivecontextpack.SourceEvidenceOriginSummary},
		GraphMaxSeedFacts:       qaGraphMaxSeedFacts,
		GraphOptions: viewentityfact.GraphExpansionOptions{
			MaxFactsPerSeed:           qaGraphMaxFactsPerSeed,
			MaxBridgeFacts:            qaGraphMaxBridgeFacts,
			MaxSourceRefsPerGraphPath: qaGraphRefsPerPath,
		},
		OriginMetadataKey: qaRetrievalOriginMetadataKey,
		OriginMetadataValues: derivecontextpack.SourceEvidenceOriginValues{
			Direct:       qaRetrievalOriginDirect,
			Summary:      qaRetrievalOriginSummary,
			EntityFact:   qaRetrievalOriginEntity,
			Graph:        qaRetrievalOriginGraph,
			Neighborhood: qaRetrievalOriginNeighbor,
		},
	}
}

func putQAGraphEntity(t *testing.T, ctx context.Context, store *viewentityfact.WorkspaceStore, scope memory.Scope, entity viewentityfact.Entity) {
	t.Helper()
	entity.Scope = scope
	if entity.Confidence == 0 {
		entity.Confidence = 0.9
	}
	if _, err := store.PutEntity(ctx, entity); err != nil {
		t.Fatalf("PutEntity(%s): %v", entity.ID, err)
	}
}

func putQAGraphFact(t *testing.T, ctx context.Context, store *viewentityfact.WorkspaceStore, scope memory.Scope, fact viewentityfact.Fact) viewentityfact.Fact {
	t.Helper()
	fact.Scope = scope
	if fact.Confidence == 0 {
		fact.Confidence = 0.9
	}
	stored, err := store.PutFact(ctx, fact)
	if err != nil {
		t.Fatalf("PutFact(%s): %v", fact.ID, err)
	}
	return stored
}

func countContextMessagesByID(pack *memory.ContextPack, id string) int {
	count := 0
	for _, item := range renderableContextItems(pack) {
		if item.Message != nil && item.Message.ID == id {
			count++
		}
	}
	return count
}

func qaTestMessageRef(conversationID, messageID string) views.SourceRef {
	return views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: conversationID,
			MessageID:      messageID,
		},
	}
}

func countContextMessagesByIDAndOrigin(pack *memory.ContextPack, id, origin string) int {
	count := 0
	for _, item := range renderableContextItems(pack) {
		if item.Message != nil && item.Message.ID == id && formatContextMetadataValue(contextItemMetadataValue(item, qaRetrievalOriginMetadataKey)) == origin {
			count++
		}
	}
	return count
}
