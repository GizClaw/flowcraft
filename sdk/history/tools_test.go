package history

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestMemoryExpandTool_NoConversationID(t *testing.T) {
	tool := newHistoryExpandTool(ToolDeps{})
	_, err := tool.Execute(context.Background(), `{"summary_id":"n1"}`)
	if err == nil || !strings.Contains(err.Error(), "no conversation ID") {
		t.Fatalf("expected no conversation ID error, got: %v", err)
	}
}

func TestMemoryCompactTool_Definition(t *testing.T) {
	tool := newHistoryCompactTool(ToolDeps{})
	def := tool.Definition()
	if def.Name != "history_compact" {
		t.Fatalf("expected name history_compact, got %s", def.Name)
	}
}

func TestConversationIDContext(t *testing.T) {
	ctx := context.Background()
	if id := ConversationIDFrom(ctx); id != "" {
		t.Fatal("expected empty")
	}

	ctx = WithConversationID(ctx, "test-123")
	if id := ConversationIDFrom(ctx); id != "test-123" {
		t.Fatalf("expected test-123, got %q", id)
	}
}

func TestFileStore_GetMessageRange(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	convID := "range-test"

	msgs := make([]model.Message, 20)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "msg")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	// InMemoryStore doesn't implement RangeReader, so test FileStore.
	// Just verify the interface is defined.
	var _ RangeReader = (*FileStore)(nil)
}

// --- RegisterTools / Definition ---

func TestRegisterTools_AddsBothToolsWithCorrectScopes(t *testing.T) {
	registry := tool.NewRegistry()
	RegisterTools(registry, ToolDeps{})

	expand, ok := registry.Get("history_expand")
	if !ok {
		t.Fatal("expected history_expand to be registered")
	}
	if registry.ScopeOf("history_expand") != tool.ScopeAgent {
		t.Fatalf("history_expand scope = %s, want agent", registry.ScopeOf("history_expand"))
	}
	if def := expand.Definition(); def.Name != "history_expand" {
		t.Fatalf("history_expand definition Name = %q", def.Name)
	}

	compact, ok := registry.Get("history_compact")
	if !ok {
		t.Fatal("expected history_compact to be registered")
	}
	if registry.ScopeOf("history_compact") != tool.ScopePlatform {
		t.Fatalf("history_compact scope = %s, want platform", registry.ScopeOf("history_compact"))
	}
	if def := compact.Definition(); def.Name != "history_compact" {
		t.Fatalf("history_compact definition Name = %q", def.Name)
	}
}

func TestHistoryExpandTool_Definition_HasRequiredField(t *testing.T) {
	def := newHistoryExpandTool(ToolDeps{}).Definition()
	if def.Name != "history_expand" {
		t.Fatalf("name = %q", def.Name)
	}
	if def.Description == "" {
		t.Fatal("expected non-empty description")
	}
}

// --- history_expand.Execute ---

func TestHistoryExpandTool_Execute_BadJSON(t *testing.T) {
	tl := newHistoryExpandTool(ToolDeps{})
	_, err := tl.Execute(context.Background(), "{not json")
	if err == nil || !strings.Contains(err.Error(), "parse args") {
		t.Fatalf("expected parse-args error, got %v", err)
	}
}

func TestHistoryExpandTool_Execute_NoSummaryStore(t *testing.T) {
	tl := newHistoryExpandTool(ToolDeps{})
	ctx := WithConversationID(context.Background(), "c1")
	_, err := tl.Execute(ctx, `{"summary_id":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "summary store not available") {
		t.Fatalf("expected summary-store error, got %v", err)
	}
}

func TestHistoryExpandTool_Execute_ExpandsLeafFromMessages(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	fileStore := NewFileStore(ws, "memory")
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}

	ctx := WithConversationID(context.Background(), "convX")
	for _, text := range []string{"first", "second", "third"} {
		existing, _ := fileStore.GetMessages(ctx, "convX")
		_ = fileStore.SaveMessages(ctx, "convX", append(existing,
			model.NewTextMessage(model.RoleUser, text),
		))
	}

	leaf := &SummaryNode{
		ConversationID: "convX",
		Depth:          0,
		Content:        "leaf",
		EarliestSeq:    0,
		LatestSeq:      2,
	}
	_ = summaryStore.Save(ctx, leaf)

	tl := newHistoryExpandTool(ToolDeps{
		SummaryStore: summaryStore,
		MessageStore: fileStore, // FileStore implements RangeReader
		Workspace:    ws,
		Prefix:       "memory",
		Config:       DefaultDAGConfig(),
	})

	out, err := tl.Execute(ctx, `{"summary_id":"`+leaf.ID+`","max_messages":10}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "first") || !strings.Contains(out, "third") {
		t.Fatalf("expected expanded message text, got %q", out)
	}
}

func TestHistoryExpandTool_Execute_NonLeafFormatsChildren(t *testing.T) {
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ctx := WithConversationID(context.Background(), "convY")

	// Two depth-0 leaves and one depth-1 node referencing them.
	leafA := &SummaryNode{ConversationID: "convY", Depth: 0, Content: "child-a", EarliestSeq: 0, LatestSeq: 4, ExpandHint: "[Expand for details about: a]"}
	leafB := &SummaryNode{ConversationID: "convY", Depth: 0, Content: "child-b", EarliestSeq: 5, LatestSeq: 9}
	_ = summaryStore.Save(ctx, leafA)
	_ = summaryStore.Save(ctx, leafB)

	parent := &SummaryNode{
		ConversationID: "convY",
		Depth:          1,
		Content:        "parent",
		SourceIDs:      []string{leafA.ID, leafB.ID, "missing-id"},
		EarliestSeq:    0,
		LatestSeq:      9,
	}
	_ = summaryStore.Save(ctx, parent)

	tl := newHistoryExpandTool(ToolDeps{SummaryStore: summaryStore})
	out, err := tl.Execute(ctx, `{"summary_id":"`+parent.ID+`"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "child-a") || !strings.Contains(out, "child-b") {
		t.Fatalf("expected both children rendered, got %q", out)
	}
	if !strings.Contains(out, "[Expand for details about: a]") {
		t.Fatalf("expected ExpandHint to be rendered, got %q", out)
	}
}

func TestHistoryExpandTool_Execute_GetByConvIDError(t *testing.T) {
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ctx := WithConversationID(context.Background(), "convZ")

	tl := newHistoryExpandTool(ToolDeps{SummaryStore: summaryStore})
	_, err := tl.Execute(ctx, `{"summary_id":"missing"}`)
	if err == nil || !strings.Contains(err.Error(), "history_expand") {
		t.Fatalf("expected wrapped not-found error, got %v", err)
	}
}

func TestFormatChildSummaries_Format(t *testing.T) {
	out := formatChildSummaries([]*SummaryNode{
		{Depth: 0, EarliestSeq: 0, LatestSeq: 4, Content: "child0", ExpandHint: "[Expand for details about: a]"},
		{Depth: 1, EarliestSeq: 5, LatestSeq: 9, Content: "child1"},
	})
	if !strings.Contains(out, "[d0 seq 0-4] child0") {
		t.Fatalf("missing depth/seq header: %q", out)
	}
	if !strings.Contains(out, "[Expand for details about: a]") {
		t.Fatalf("missing expand hint: %q", out)
	}
	if !strings.Contains(out, "[d1 seq 5-9] child1") {
		t.Fatalf("missing second child: %q", out)
	}
	if formatChildSummaries(nil) != "" {
		t.Fatal("expected empty string for nil input")
	}
}

// --- history_compact.Execute fallback path (no Coordinator wired) ---

func TestHistoryCompactTool_Execute_BadJSON(t *testing.T) {
	tl := newHistoryCompactTool(ToolDeps{})
	_, err := tl.Execute(context.Background(), "{nope")
	if err == nil || !strings.Contains(err.Error(), "parse args") {
		t.Fatalf("expected parse-args error, got %v", err)
	}
}

func TestHistoryCompactTool_Execute_FallbackPathCompactOnly(t *testing.T) {
	// No Coordinator + no Workspace → only the compact branch runs; the
	// archive branch is silently skipped because deps.Workspace is nil.
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ctx := context.Background()
	_ = summaryStore.Save(ctx, &SummaryNode{ConversationID: "c", Depth: 0, Content: "leaf", EarliestSeq: 0, LatestSeq: 5})

	tl := newHistoryCompactTool(ToolDeps{
		SummaryStore: summaryStore,
		Config:       DefaultDAGConfig(),
	})
	out, err := tl.Execute(ctx, `{"conversation_id":"c"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var parsed struct {
		CompactResult *CompactResult `json:"compact_result,omitempty"`
		ArchiveResult *ArchiveResult `json:"archive_result,omitempty"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, out)
	}
	if parsed.CompactResult == nil {
		t.Fatal("expected compact_result to be populated")
	}
	if parsed.ArchiveResult != nil {
		t.Fatal("expected archive_result to be skipped without workspace")
	}
}

func TestHistoryCompactTool_Execute_FallbackPathArchiveOnly(t *testing.T) {
	// Skip compact (compact:false), keep archive — exercises the second
	// branch of the fallback path when Workspace + MessageStore are set.
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(ws, "memory")
	ctx := context.Background()
	convID := "compact-tool-archive"

	msgs := make([]model.Message, 20)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "x")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	cfg := DefaultDAGConfig()
	cfg.Archive.ArchiveThreshold = 15
	cfg.Archive.ArchiveBatchSize = 10
	tl := newHistoryCompactTool(ToolDeps{
		MessageStore: store,
		Workspace:    ws,
		Prefix:       "memory",
		Config:       cfg,
	})

	out, err := tl.Execute(ctx, `{"conversation_id":"`+convID+`","compact":false}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var parsed struct {
		ArchiveResult *ArchiveResult `json:"archive_result,omitempty"`
		CompactResult *CompactResult `json:"compact_result,omitempty"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.CompactResult != nil {
		t.Fatal("expected compact to be skipped when compact:false")
	}
	if parsed.ArchiveResult == nil {
		t.Fatal("expected archive_result to be populated")
	}
	if parsed.ArchiveResult.MessagesArchived != 10 {
		t.Fatalf("expected 10 archived, got %d", parsed.ArchiveResult.MessagesArchived)
	}
}
