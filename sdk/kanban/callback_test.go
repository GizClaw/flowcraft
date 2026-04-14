package kanban

import (
	"strings"
	"testing"
	"time"
)

func TestBuildCallbackQuery_Success(t *testing.T) {
	card := &Card{
		ID:     "card-001",
		Status: CardDone,
		Payload: map[string]any{
			"target_agent_id": "copilot_builder",
			"query":           "create RAG app",
			"output":          "已成功创建 RAG 应用",
			"user_query":      "帮我创建 RAG 应用",
			"dispatch_note":   "完成后告知用户",
		},
	}
	result := &ResultPayload{Output: "已成功创建 RAG 应用"}

	msg := BuildCallbackQuery(card, result)

	for _, want := range []string{
		"[Task Callback]",
		"card-001",
		"copilot_builder",
		"Status: completed",
		"已成功创建 RAG 应用",
		"task_context",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("callback missing %q, got:\n%s", want, msg)
		}
	}
}

func TestBuildCallbackQuery_Failure(t *testing.T) {
	card := &Card{
		ID:     "card-002",
		Status: CardFailed,
		Error:  "compile error",
		Payload: map[string]any{
			"target_agent_id": "copilot_builder",
			"query":           "create app",
		},
	}
	result := &ResultPayload{Error: "compile error"}

	msg := BuildCallbackQuery(card, result)

	if !strings.Contains(msg, "Status: failed") {
		t.Errorf("expected failure status, got:\n%s", msg)
	}
	if !strings.Contains(msg, "compile error") {
		t.Errorf("expected error in message, got:\n%s", msg)
	}
}

func TestBuildCallbackQuery_Truncation(t *testing.T) {
	long := strings.Repeat("x", 300)
	card := &Card{
		ID:      "card-003",
		Status:  CardDone,
		Payload: map[string]any{"target_agent_id": "t"},
	}
	result := &ResultPayload{Output: long}

	msg := BuildCallbackQuery(card, result)

	if strings.Contains(msg, long) {
		t.Error("expected output to be truncated")
	}
	if !strings.Contains(msg, "...") {
		t.Error("expected truncation marker")
	}
}

func TestBuildTaskContext_Full(t *testing.T) {
	card := &Card{
		ID:     "card-010",
		Status: CardDone,
		Payload: map[string]any{
			"target_agent_id": "copilot_builder",
			"query":           "在应用 xxx 中创建 RAG 图",
			"output":          "成功创建 4 个节点",
			"user_query":      "帮我创建一个 RAG 应用",
			"dispatch_note":   "完成后告知用户应用名称",
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	ctx := BuildTaskContext(card)

	for _, want := range []string{
		"## Task Context (card-010)",
		"### Original Request",
		"帮我创建一个 RAG 应用",
		"### Dispatch Note",
		"完成后告知用户应用名称",
		"### Task Instruction",
		"copilot_builder",
		"在应用 xxx 中创建 RAG 图",
		"### Execution Result",
		"成功创建 4 个节点",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context missing %q, got:\n%s", want, ctx)
		}
	}
}

func TestBuildTaskContext_Pending(t *testing.T) {
	card := &Card{
		ID:     "card-pending",
		Status: CardPending,
		Payload: TaskPayload{
			TargetAgentID: "copilot_builder",
			Query:         "create app",
			UserQuery:     "帮我创建应用",
		},
	}

	ctx := BuildTaskContext(card)

	if !strings.Contains(ctx, "Status: pending") {
		t.Errorf("pending card should show waiting status, got:\n%s", ctx)
	}
	if strings.Contains(ctx, "Status: completed") {
		t.Errorf("pending card should NOT show completed status, got:\n%s", ctx)
	}
}

func TestBuildTaskContext_Claimed(t *testing.T) {
	card := &Card{
		ID:     "card-claimed",
		Status: CardClaimed,
		Payload: TaskPayload{
			TargetAgentID: "copilot_builder",
			Query:         "build workflow",
			UserQuery:     "帮我构建",
		},
	}

	ctx := BuildTaskContext(card)

	if !strings.Contains(ctx, "Status: running") {
		t.Errorf("claimed card should show running status, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "do not resubmit") {
		t.Errorf("claimed card should warn against resubmission, got:\n%s", ctx)
	}
}

func TestBuildTaskContext_Failed(t *testing.T) {
	card := &Card{
		ID:     "card-011",
		Status: CardFailed,
		Error:  "edge from unknown node",
		Payload: map[string]any{
			"target_agent_id": "copilot_builder",
			"query":           "build graph",
			"user_query":      "创建应用",
		},
	}

	ctx := BuildTaskContext(card)

	if !strings.Contains(ctx, "Status: failed") {
		t.Errorf("expected failure status, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "edge from unknown node") {
		t.Errorf("expected error detail, got:\n%s", ctx)
	}
}

func TestBuildTaskContext_NoOptionalFields(t *testing.T) {
	card := &Card{
		ID:     "card-012",
		Status: CardDone,
		Payload: map[string]any{
			"target_agent_id": "copilot_runner",
			"query":           "run workflow",
			"output":          "ok",
		},
	}

	ctx := BuildTaskContext(card)

	if strings.Contains(ctx, "### Original Request") {
		t.Error("should not contain user query section when empty")
	}
	if strings.Contains(ctx, "### Dispatch Note") {
		t.Error("should not contain dispatch note section when empty")
	}
	if !strings.Contains(ctx, "copilot_runner") {
		t.Errorf("expected target_agent_id, got:\n%s", ctx)
	}
}

func TestPayloadMap_Struct(t *testing.T) {
	tp := TaskPayload{
		TargetAgentID: "tpl",
		Query:         "q",
		UserQuery:     "uq",
		DispatchNote:  "dn",
	}
	m := PayloadMap(tp)
	if m["target_agent_id"] != "tpl" {
		t.Fatalf("expected tpl, got %v", m["target_agent_id"])
	}
	if m["user_query"] != "uq" {
		t.Fatalf("expected uq, got %v", m["user_query"])
	}
}

func TestPayloadMap_Map(t *testing.T) {
	m := PayloadMap(map[string]any{"target_agent_id": "x", "output": "y"})
	if m["target_agent_id"] != "x" || m["output"] != "y" {
		t.Fatalf("unexpected map: %v", m)
	}
}

func TestPayloadMap_Nil(t *testing.T) {
	m := PayloadMap(nil)
	if m != nil {
		t.Fatalf("expected nil, got %v", m)
	}
}

func TestCompactCallbackForMemory_Short(t *testing.T) {
	msg := "[Task Callback] card_id=c1\nStatus: completed"
	got := CompactCallbackForMemory(msg, 300)
	if got != msg {
		t.Errorf("short message should not be truncated, got:\n%q", got)
	}
}

func TestCompactCallbackForMemory_Long(t *testing.T) {
	var b strings.Builder
	b.WriteString("[Task Callback] card_id=c1\n")
	b.WriteString("Status: completed\n")
	b.WriteString("Summary: " + strings.Repeat("x", 500) + "\n")
	msg := b.String()

	got := CompactCallbackForMemory(msg, 100)
	if len(got) > 200 {
		t.Errorf("expected compact output, got %d bytes", len(got))
	}
	if !strings.Contains(got, "[Task Callback]") {
		t.Error("should preserve header")
	}
	if !strings.Contains(got, "task_context") {
		t.Error("should include task_context hint")
	}
}

func TestIsCallbackMessage(t *testing.T) {
	if !IsCallbackMessage("[Task Callback] card_id=c1") {
		t.Error("should detect callback message")
	}
	if IsCallbackMessage("普通消息") {
		t.Error("should not detect non-callback message")
	}
}
