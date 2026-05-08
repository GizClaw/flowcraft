package kanban_test

import (
	"context"
	"strings"
	"testing"

	sdkkanban "github.com/GizClaw/flowcraft/sdk/kanban"
	tool "github.com/GizClaw/flowcraft/sdkx/tool/kanban"
)

func newKanban(t *testing.T) *sdkkanban.Kanban {
	t.Helper()
	sb := sdkkanban.NewBoard("scope-test")
	k := sdkkanban.New(context.Background(), sb,
		sdkkanban.WithConfig(sdkkanban.KanbanConfig{MaxPendingTasks: 100}))
	t.Cleanup(k.Stop)
	return k
}

func TestSubmitTool_Definition(t *testing.T) {
	x := &tool.SubmitTool{}
	def := x.Definition()
	if def.Name != "kanban_submit" {
		t.Fatalf("name = %q, want kanban_submit", def.Name)
	}
}

func TestSubmitTool_Execute(t *testing.T) {
	k := newKanban(t)
	x := &tool.SubmitTool{Kanban: k}
	out, err := x.Execute(context.Background(),
		`{"query":"create app","target_agent_id":"copilot_builder","user_query":"创建应用","dispatch_note":"完成后通知"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{`"card_id"`, `"status":"submitted"`, `"target_agent_id":"copilot_builder"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in result: %s", want, out)
		}
	}
}

func TestSubmitTool_NilKanban(t *testing.T) {
	x := &tool.SubmitTool{}
	_, err := x.Execute(context.Background(), `{"query":"q","target_agent_id":"a"}`)
	if err == nil {
		t.Fatal("want error when no kanban available")
	}
}

func TestSubmitTool_FromContext(t *testing.T) {
	k := newKanban(t)
	ctx := tool.WithKanban(context.Background(), k)
	x := &tool.SubmitTool{}
	out, err := x.Execute(ctx, `{"query":"q","target_agent_id":"a"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, `"card_id"`) {
		t.Errorf("missing card_id: %s", out)
	}
}

// Round-trip with sdk-side WithKanban: contexts installed via the
// deprecated sdk helper must be readable by the sdkx KanbanFrom and
// vice-versa during the v0.2.x → v0.3.0 transition.
func TestContextInterop(t *testing.T) {
	k := newKanban(t)

	// sdk-installed → sdkx readable
	ctx := sdkkanban.WithKanban(context.Background(), k)
	if got := tool.KanbanFrom(ctx); got != k {
		t.Errorf("sdk install / sdkx read: got %v want %v", got, k)
	}

	// sdkx-installed → sdk readable
	ctx2 := tool.WithKanban(context.Background(), k)
	if got := sdkkanban.KanbanFrom(ctx2); got != k {
		t.Errorf("sdkx install / sdk read: got %v want %v", got, k)
	}
}

func TestTaskContextTool_NilKanban(t *testing.T) {
	x := &tool.TaskContextTool{}
	_, err := x.Execute(context.Background(), `{"card_id":"x"}`)
	if err == nil {
		t.Fatal("want error when no kanban available")
	}
}
