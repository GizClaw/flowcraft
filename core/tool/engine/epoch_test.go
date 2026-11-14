package engine_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/engine"
)

func epochAssembly(tb testing.TB, dynamic bool) *engine.Assembly {
	tb.Helper()
	opts := []engine.AssemblyOption{}
	if dynamic {
		opts = append(opts, engine.WithDynamic(engine.Policy{Default: engine.ExposureDeferred}))
	}
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{funcTool("read_file", "reads")}}},
		opts...,
	)
	if err != nil {
		tb.Fatalf("NewAssembly: %v", err)
	}
	return assembly
}

// Every mutation that can change the visible set must move the epoch, or a
// caller's cached toolset goes stale — a correctness bug, not a slow path.
func TestEpochMovesOnMutationsOnly(t *testing.T) {
	view := epochAssembly(t, true).NewCatalogView()
	start := view.Epoch()

	_ = view.Definitions()
	if got := view.Epoch(); got != start {
		t.Fatalf("Definitions moved the epoch %d -> %d", start, got)
	}
	view.AdvanceRound()
	if got := view.Epoch(); got == start {
		t.Fatal("AdvanceRound left the epoch unchanged")
	}
	before := view.Epoch()
	view.Discover("read_file")
	if got := view.Epoch(); got == before {
		t.Fatal("Discover left the epoch unchanged")
	}
	before = view.Epoch()
	call, err := message.NewToolCall("c1", "read_file", map[string]any{"file_path": "/tmp/x"})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	view.RecordCall(call)
	if got := view.Epoch(); got == before {
		t.Fatal("RecordCall left the epoch unchanged")
	}
}

func TestStaticViewEpochIsConstant(t *testing.T) {
	view := epochAssembly(t, false).NewCatalogView()
	_ = view.Definitions()
	if got := view.Epoch(); got != 0 {
		t.Fatalf("static epoch = %d, want 0", got)
	}
}

func BenchmarkDefinitionsDynamic(b *testing.B) {
	view := epochAssembly(b, true).NewCatalogView()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = view.Definitions()
	}
}
