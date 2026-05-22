package history

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// makeMsg is a tiny constructor that keeps the table-driven tests below
// readable by collapsing the verbose Parts boilerplate.
func makeMsg(role model.Role, text string) model.Message {
	return model.NewTextMessage(role, text)
}

func makeToolCallMsg(name string) model.Message {
	return model.Message{
		Role: model.RoleAssistant,
		Parts: []model.Part{
			{Type: model.PartText, Text: "calling " + name},
			{Type: model.PartToolCall, ToolCall: &model.ToolCall{Name: name, ID: "tc1"}},
		},
	}
}

func makeToolResultMsg() model.Message {
	return model.Message{
		Role: model.RoleTool,
		Parts: []model.Part{
			{Type: model.PartToolResult, ToolResult: &model.ToolResult{ToolCallID: "tc1", Content: "ok"}},
		},
	}
}

func TestApplyLoadOptions_Empty(t *testing.T) {
	t.Parallel()
	msgs := []model.Message{
		makeMsg(model.RoleUser, "hi"),
		makeMsg(model.RoleAssistant, "hello"),
	}
	got := ApplyLoadOptions(msgs, LoadOptions{})
	if !reflect.DeepEqual(got, msgs) {
		t.Fatalf("zero LoadOptions must be a no-op, got %+v", got)
	}
}

func TestApplyLoadOptions_Roles(t *testing.T) {
	t.Parallel()
	msgs := []model.Message{
		makeMsg(model.RoleSystem, "sys"),
		makeMsg(model.RoleUser, "u1"),
		makeMsg(model.RoleAssistant, "a1"),
		makeMsg(model.RoleUser, "u2"),
	}
	got := ApplyLoadOptions(msgs, LoadOptions{Roles: []model.Role{model.RoleUser}})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2; got %+v", len(got), got)
	}
	for _, m := range got {
		if m.Role != model.RoleUser {
			t.Fatalf("unexpected role %s", m.Role)
		}
	}
}

func TestApplyLoadOptions_SinceSeq(t *testing.T) {
	t.Parallel()
	msgs := []model.Message{
		makeMsg(model.RoleUser, "0"),
		makeMsg(model.RoleAssistant, "1"),
		makeMsg(model.RoleUser, "2"),
		makeMsg(model.RoleAssistant, "3"),
	}
	got := ApplyLoadOptions(msgs, LoadOptions{SinceSeq: 2})
	if len(got) != 2 || got[0].Content() != "2" || got[1].Content() != "3" {
		t.Fatalf("SinceSeq cutoff wrong: %+v", got)
	}
}

func TestApplyLoadOptions_LimitN(t *testing.T) {
	t.Parallel()
	msgs := []model.Message{
		makeMsg(model.RoleUser, "1"),
		makeMsg(model.RoleAssistant, "2"),
		makeMsg(model.RoleUser, "3"),
		makeMsg(model.RoleAssistant, "4"),
	}
	got := ApplyLoadOptions(msgs, LoadOptions{LimitN: 2})
	if len(got) != 2 || got[0].Content() != "3" || got[1].Content() != "4" {
		t.Fatalf("LimitN must keep the tail, got %+v", got)
	}
}

func TestApplyLoadOptions_StripsToolsByDefault(t *testing.T) {
	t.Parallel()
	msgs := []model.Message{
		makeMsg(model.RoleUser, "search docs"),
		makeToolCallMsg("search"),
		makeToolResultMsg(),
		makeMsg(model.RoleAssistant, "found it"),
	}
	got := ApplyLoadOptions(msgs, LoadOptions{})
	// Sanity: zero LoadOptions is no-op even when tool messages exist.
	if len(got) != 4 {
		t.Fatalf("zero opts = no-op, got len=%d", len(got))
	}

	// Now actively strip tools via a Roles filter (so opts is non-zero
	// and IncludeTools=false takes effect).
	got = ApplyLoadOptions(msgs, LoadOptions{Roles: []model.Role{model.RoleUser, model.RoleAssistant}})
	// Tool-result message (RoleTool) is gone; tool-call assistant
	// message keeps its text part ("calling search") and drops the
	// PartToolCall.
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d (%+v)", len(got), got)
	}
	for _, m := range got {
		for _, p := range m.Parts {
			if p.Type == model.PartToolCall || p.Type == model.PartToolResult {
				t.Fatalf("tool part leaked through: %+v", m)
			}
		}
	}
}

func TestApplyLoadOptions_IncludeToolsKeepsEverything(t *testing.T) {
	t.Parallel()
	msgs := []model.Message{
		makeMsg(model.RoleUser, "search"),
		makeToolCallMsg("search"),
		makeToolResultMsg(),
		makeMsg(model.RoleAssistant, "done"),
	}
	got := ApplyLoadOptions(msgs, LoadOptions{Roles: []model.Role{model.RoleUser, model.RoleAssistant, model.RoleTool}, IncludeTools: true})
	if len(got) != 4 {
		t.Fatalf("expected all 4 messages, got %d", len(got))
	}
	if !got[1].HasToolCalls() {
		t.Fatal("tool-call must be preserved when IncludeTools=true")
	}
}

func TestLoadFiltered_FallbackToBuffer(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()
	hist := NewBuffer(store)

	convID := "c1"
	if err := hist.Append(context.Background(), convID, []model.Message{
		makeMsg(model.RoleSystem, "sys"),
		makeMsg(model.RoleUser, "q1"),
		makeMsg(model.RoleAssistant, "a1"),
		makeMsg(model.RoleUser, "q2"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := LoadFiltered(context.Background(), hist, convID, LoadOptions{
		Roles:  []model.Role{model.RoleUser},
		LimitN: 1,
	})
	if err != nil {
		t.Fatalf("LoadFiltered: %v", err)
	}
	if len(got) != 1 || got[0].Content() != "q2" {
		t.Fatalf("expected last user message q2, got %+v", got)
	}
}
