package localmem

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	locomosource "github.com/GizClaw/flowcraft/eval/locomo/source"
	"github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

func TestBuildSyncWorkspace(t *testing.T) {
	root := t.TempDir()
	mem, closeMem, err := Build(MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() {
		if err := closeMem(); err != nil {
			t.Fatalf("close memory: %v", err)
		}
	}()
	for _, stage := range mem.Plan().Write {
		if stage.Async {
			t.Fatalf("write stage %s is async", stage.Name)
		}
	}
	if got := mem.Plan().Write; len(got) != 4 || got[0].Name != "append_message" || got[1].Name != "index_messages" || got[2].Name != "build_summary_dag" || got[3].Name != "build_entity_facts" {
		t.Fatalf("write plan = %+v, want raw append plus message indexing, summary dag, and entity facts", got)
	}
	if got := mem.Plan().Read; len(got) != 5 || got[0].Name != "load_recent_messages" || got[1].Name != "retrieve_messages" || got[2].Name != "retrieve_summaries" || got[3].Name != "retrieve_entity_facts" || got[4].Name != "pack_context" {
		t.Fatalf("read plan = %+v, want raw recent plus message, summary, and entity retrieval plan", got)
	}
	if got := mem.Plan().Read[2].Config; got["drilldown_max_depth"] != 0 || got["drilldown_child_top_k"] != 2 {
		t.Fatalf("retrieve_summaries config = %+v, want layered SummaryDAG drill-down config", got)
	}
	sample := dataset.Sample{
		ID: "conv-a",
		Sessions: []dataset.Session{{
			Index:    1,
			DateTime: "2024-01-01 09:00",
			Turns: []dataset.Turn{
				{DiaID: "d1", Speaker: "Ada", Text: "Ada likes tea."},
				{DiaID: "d2", Speaker: "Ben", Text: "Ben mentioned coffee."},
				{DiaID: "d3", Speaker: "Ada", Text: "Ada packed a blue notebook."},
				{DiaID: "d4", Speaker: "Ben", Text: "Ben watered the basil."},
				{DiaID: "d5", Speaker: "Ada", Text: "Ada bought train tickets."},
				{DiaID: "d6", Speaker: "Ben", Text: "Ben found the spare umbrella."},
				{DiaID: "d7", Speaker: "Ada", Text: "Ada planned dinner."},
			},
		}},
	}
	scope := locomosource.SampleScope("run-test", "synthetic", sample)
	if err := locomosource.IngestSession(context.Background(), mem, scope, sample.Sessions[0]); err != nil {
		t.Fatalf("IngestSession: %v", err)
	}
	pack, err := mem.PackContext(context.Background(), memory.ContextRequest{Scope: scope, Query: "Ada tea", TopK: 5})
	if err != nil {
		t.Fatalf("PackContext: %v", err)
	}
	if len(pack.Window.Messages) == 0 {
		t.Fatal("PackContext window is empty")
	}
	if len(pack.MessageHits) == 0 {
		t.Fatal("PackContext message retrieval hits are empty")
	}
	if len(pack.SummaryHits) == 0 {
		t.Fatal("PackContext summary retrieval hits are empty")
	}
	if _, err := os.Stat(filepath.Join(root, "sources/message")); err != nil {
		t.Fatalf("message workspace dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "retrieval")); err != nil {
		t.Fatalf("retrieval workspace dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "views/summary_dag")); err != nil {
		t.Fatalf("summary dag workspace dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "views/entity_facts")); err != nil {
		t.Fatalf("entity facts workspace dir: %v", err)
	}
}

func TestBuildUsesMemoryLLMForSummaryDAG(t *testing.T) {
	root := t.TempDir()
	fake := &fakeSummaryLLM{reply: `{"summary":"Ada likes tea and Ben mentioned coffee."}`}
	mem, closeMem, err := Build(MemoryOptions{
		WorkspaceRoot: root,
		MemoryLLM:     fake,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() {
		if err := closeMem(); err != nil {
			t.Fatalf("close memory: %v", err)
		}
	}()
	sample := dataset.Sample{
		ID: "conv-a",
		Sessions: []dataset.Session{{
			Index:    1,
			DateTime: "2024-01-01 09:00",
			Turns: []dataset.Turn{
				{DiaID: "d1", Speaker: "Ada", Text: "Ada likes tea."},
				{DiaID: "d2", Speaker: "Ben", Text: "Ben mentioned coffee."},
				{DiaID: "d3", Speaker: "Ada", Text: "Ada packed a blue notebook."},
				{DiaID: "d4", Speaker: "Ben", Text: "Ben watered the basil."},
				{DiaID: "d5", Speaker: "Ada", Text: "Ada bought train tickets."},
				{DiaID: "d6", Speaker: "Ben", Text: "Ben found the spare umbrella."},
				{DiaID: "d7", Speaker: "Ada", Text: "Ada planned dinner."},
				{DiaID: "d8", Speaker: "Ben", Text: "Ben charged the camera."},
				{DiaID: "d9", Speaker: "Ada", Text: "Ada chose the museum route."},
				{DiaID: "d10", Speaker: "Ben", Text: "Ben reserved a taxi."},
				{DiaID: "d11", Speaker: "Ada", Text: "Ada packed jasmine tea."},
				{DiaID: "d12", Speaker: "Ben", Text: "Ben printed coffee coupons."},
				{DiaID: "d13", Speaker: "Ada", Text: "Ada updated the itinerary."},
				{DiaID: "d14", Speaker: "Ben", Text: "Ben confirmed the hotel."},
				{DiaID: "d15", Speaker: "Ada", Text: "Ada bought museum tickets."},
				{DiaID: "d16", Speaker: "Ben", Text: "Ben packed rain jackets."},
				{DiaID: "d17", Speaker: "Ada", Text: "Ada kept the tea receipt."},
			},
		}},
	}
	scope := locomosource.SampleScope("run-test", "synthetic", sample)
	if err := locomosource.IngestSession(context.Background(), mem, scope, sample.Sessions[0]); err != nil {
		t.Fatalf("IngestSession: %v", err)
	}
	if len(fake.calls) == 0 {
		t.Fatal("memory LLM was not called for SummaryDAG")
	}
	pack, err := mem.PackContext(context.Background(), memory.ContextRequest{Scope: scope, Query: "Ada tea coffee", TopK: 10})
	if err != nil {
		t.Fatalf("PackContext: %v", err)
	}
	if len(pack.SummaryHits) == 0 {
		t.Fatal("PackContext summary retrieval hits are empty")
	}
	sawLLM := false
	sawCondensed := false
	for _, hit := range pack.SummaryHits {
		if got := hit.Node.Metadata["algorithm"]; got == "summary_llm" {
			sawLLM = true
		}
		if hit.Node.Metadata["node_kind"] == "condensed" && hit.Node.Level == 1 && len(hit.Node.ParentIDs) == 4 {
			sawCondensed = true
		}
	}
	if !sawLLM {
		t.Fatalf("summary hits = %+v, want summary_llm algorithm", pack.SummaryHits)
	}
	if !sawCondensed {
		t.Fatalf("summary hits = %+v, want layered SummaryDAG condensed node when drill-down is disabled", pack.SummaryHits)
	}
}

type fakeSummaryLLM struct {
	reply string
	calls [][]llm.Message
}

func (f *fakeSummaryLLM) Generate(_ context.Context, messages []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	f.calls = append(f.calls, append([]llm.Message(nil), messages...))
	return llm.NewTextMessage(llm.RoleAssistant, f.reply), llm.TokenUsage{}, nil
}

func (f *fakeSummaryLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}
