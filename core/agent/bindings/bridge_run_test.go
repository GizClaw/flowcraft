package bindings

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
)

func runGetters(t *testing.T, ctx context.Context) map[string]func() string {
	t.Helper()
	name, raw := NewRunInfoBridge()(ctx)
	if name != "run" {
		t.Fatalf("binding name = %q, want %q", name, "run")
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("binding value = %T, want map[string]any", raw)
	}
	out := make(map[string]func() string, len(m))
	for k, v := range m {
		fn, ok := v.(func() string)
		if !ok {
			t.Fatalf("run.%s = %T, want func() string", k, v)
		}
		out[k] = fn
	}
	return out
}

func TestRunInfoBridge_ReadsAmbientIdentity(t *testing.T) {
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{
		Identity: agent.Identity{
			AgentID:        "researcher",
			RunID:          "run-1",
			ParentRunID:    "run-0",
			TaskID:         "task-9",
			ConversationID: "conv-3",
		},
	})

	get := runGetters(t, ctx)
	want := map[string]string{
		"get_run_id":        "run-1",
		"get_task_id":       "task-9",
		"get_agent_id":      "researcher",
		"get_context_id":    "conv-3",
		"get_parent_run_id": "run-0",
	}
	for name, w := range want {
		if got := get[name](); got != w {
			t.Errorf("%s() = %q, want %q", name, got, w)
		}
	}
}

func TestRunInfoBridge_NoRunInfoYieldsEmptyStrings(t *testing.T) {
	// Outside an engine run (or an engine that did not populate the
	// context) every getter degrades to "" rather than panicking.
	get := runGetters(t, context.Background())
	for name, fn := range get {
		if got := fn(); got != "" {
			t.Errorf("%s() = %q, want empty", name, got)
		}
	}
}
