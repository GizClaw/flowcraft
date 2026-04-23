package kanban

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSubmitTool(t *testing.T) {
	sb := NewBoard("scope-t1")
	k := New(context.Background(), sb, WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	defer k.Stop()

	tool := &SubmitTool{Kanban: k}
	def := tool.Definition()
	if def.Name != "kanban_submit" {
		t.Fatalf("expected 'kanban_submit', got %q", def.Name)
	}

	result, err := tool.Execute(context.Background(),
		`{"query":"create app","target_agent_id":"copilot_builder","user_query":"创建应用","dispatch_note":"完成后通知"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, `"card_id"`) {
		t.Fatalf("expected card_id in result, got %q", result)
	}
	if !strings.Contains(result, `"status":"submitted"`) {
		t.Fatalf("expected submitted status, got %q", result)
	}
	if !strings.Contains(result, `"message"`) {
		t.Fatalf("expected message in result, got %q", result)
	}
	if !strings.Contains(result, `"target_agent_id":"copilot_builder"`) {
		t.Fatalf("expected target_agent_id in result, got %q", result)
	}
}

func TestSubmitTool_NilKanban(t *testing.T) {
	tool := &SubmitTool{}
	_, err := tool.Execute(context.Background(), `{"query": "test", "target_agent_id": "x"}`)
	if err == nil {
		t.Fatal("expected error when no kanban available")
	}
}

func TestSubmitTool_FromContext(t *testing.T) {
	sb := NewBoard("scope-ctx")
	k := New(context.Background(), sb, WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	defer k.Stop()

	ctx := WithKanban(context.Background(), k)
	tool := &SubmitTool{}
	result, err := tool.Execute(ctx, `{"query": "test task", "target_agent_id": "tpl1"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, `"card_id"`) {
		t.Fatalf("expected card_id in result, got %q", result)
	}
}

func TestTaskContextTool(t *testing.T) {
	sb := NewBoard("scope-tc")
	k := New(context.Background(), sb, WithConfig(KanbanConfig{MaxPendingTasks: 100}))

	ctx := context.Background()
	cardID, err := k.Submit(ctx, TaskOptions{
		TargetAgentID: "copilot_builder",
		Query:         "创建 RAG 应用",
		UserQuery:     "帮我创建一个 RAG 应用",
		DispatchNote:  "完成后告知用户应用已创建",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	k.Stop()

	tc := &TaskContextTool{Kanban: k}
	def := tc.Definition()
	if def.Name != "task_context" {
		t.Fatalf("expected 'task_context', got %q", def.Name)
	}

	result, err := tc.Execute(ctx, `{"card_id":"`+cardID+`"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, want := range []string{
		"帮我创建一个 RAG 应用",
		"完成后告知用户应用已创建",
		"copilot_builder",
		"创建 RAG 应用",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("task_context missing %q, got:\n%s", want, result)
		}
	}
}

func TestTaskContextTool_NotFound(t *testing.T) {
	sb := NewBoard("scope-tc-nf")
	k := New(context.Background(), sb)

	tc := &TaskContextTool{Kanban: k}
	_, err := tc.Execute(context.Background(), `{"card_id":"nonexistent"}`)
	if err == nil {
		t.Fatal("expected error for nonexistent card")
	}
}

func TestTaskContextTool_NilKanban(t *testing.T) {
	tc := &TaskContextTool{}
	_, err := tc.Execute(context.Background(), `{"card_id":"x"}`)
	if err == nil {
		t.Fatal("expected error when no kanban available")
	}
}

func TestKanbanSubmitCallbackTaskContextLoop(t *testing.T) {
	tb := NewBoard("scope-loop")
	k := New(context.Background(), tb, WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	defer k.Stop()

	submitTool := &SubmitTool{Kanban: k}
	result, err := submitTool.Execute(context.Background(),
		`{"query":"为用户创建一个 RAG 应用","target_agent_id":"copilot_builder","user_query":"帮我创建一个 RAG 应用","dispatch_note":"完成后总结关键步骤并回复用户"}`)
	if err != nil {
		t.Fatalf("SubmitTool.Execute: %v", err)
	}

	var submitResp map[string]any
	if err := json.Unmarshal([]byte(result), &submitResp); err != nil {
		t.Fatalf("unmarshal submit result: %v", err)
	}
	cardID, _ := submitResp["card_id"].(string)
	if cardID == "" {
		t.Fatal("expected non-empty card_id")
	}

	card, err := k.GetCard(context.Background(), cardID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	tb.Claim(card.ID, "copilot_builder")
	tb.Done(card.ID, map[string]any{
		"query":           "为用户创建一个 RAG 应用",
		"target_agent_id": "copilot_builder",
		"user_query":      "帮我创建一个 RAG 应用",
		"dispatch_note":   "完成后总结关键步骤并回复用户",
		"output":          "已创建 RAG 应用，并完成基础配置。",
	})

	doneCard, err := k.GetCard(context.Background(), cardID)
	if err != nil {
		t.Fatalf("GetCard after done: %v", err)
	}
	callback := BuildCallbackQuery(doneCard, &ResultPayload{Output: "已创建 RAG 应用，并完成基础配置。"})
	if !IsCallbackMessage(callback) {
		t.Fatalf("expected callback message, got:\n%s", callback)
	}
	if !strings.Contains(callback, `task_context(card_id="`+cardID+`")`) {
		t.Fatalf("expected callback to reference task_context(card_id=%q), got:\n%s", cardID, callback)
	}

	taskContextTool := &TaskContextTool{Kanban: k}
	contextText, err := taskContextTool.Execute(context.Background(), `{"card_id":"`+cardID+`"}`)
	if err != nil {
		t.Fatalf("TaskContextTool.Execute: %v", err)
	}
	for _, want := range []string{
		"帮我创建一个 RAG 应用",
		"完成后总结关键步骤并回复用户",
		"copilot_builder",
		"已创建 RAG 应用，并完成基础配置。",
	} {
		if !strings.Contains(contextText, want) {
			t.Fatalf("task_context missing %q, got:\n%s", want, contextText)
		}
	}
}

func TestKanbanContext(t *testing.T) {
	sb := NewBoard("scope-ctx")
	k := New(context.Background(), sb)

	ctx := WithKanban(context.Background(), k)
	got := KanbanFrom(ctx)
	if got != k {
		t.Fatal("KanbanFrom should return the injected kanban")
	}
}

func TestKanbanContext_Missing(t *testing.T) {
	got := KanbanFrom(context.Background())
	if got != nil {
		t.Fatal("KanbanFrom should return nil for empty ctx")
	}
}

func TestSubmitTool_DelayResponse(t *testing.T) {
	sb := NewBoard("scope-delay")
	sched := NewScheduler()
	k := New(context.Background(), sb, WithScheduler(sched), WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	sched.Start()
	defer k.Stop()

	tool := &SubmitTool{Kanban: k}
	result, err := tool.Execute(context.Background(),
		`{"target_agent_id":"builder","query":"delayed task","delay":"5m"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var resp map[string]string
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "delayed" {
		t.Fatalf("expected status=delayed, got %q", resp["status"])
	}
	if resp["timer_id"] == "" {
		t.Fatal("expected non-empty timer_id")
	}
	if _, ok := resp["card_id"]; ok {
		t.Fatal("should not have card_id for delayed submission")
	}
	if !strings.Contains(resp["message"], "5m") {
		t.Fatalf("expected message to mention delay, got %q", resp["message"])
	}
}

func TestSubmitTool_CronResponse(t *testing.T) {
	sb := NewBoard("scope-cron")
	sched := NewScheduler()
	k := New(context.Background(), sb, WithScheduler(sched), WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	sched.Start()
	defer k.Stop()

	tool := &SubmitTool{Kanban: k}
	result, err := tool.Execute(context.Background(),
		`{"target_agent_id":"builder","query":"cron task","cron":"0 9 * * MON-FRI","timezone":"Asia/Shanghai"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var resp map[string]string
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "scheduled" {
		t.Fatalf("expected status=scheduled, got %q", resp["status"])
	}
	if resp["schedule_id"] == "" {
		t.Fatal("expected non-empty schedule_id")
	}
	if _, ok := resp["card_id"]; ok {
		t.Fatal("should not have card_id for cron submission")
	}
	if !strings.Contains(resp["message"], "cron") {
		t.Fatalf("expected message to mention cron, got %q", resp["message"])
	}
}

func TestSubmitTool_ImmediateResponse(t *testing.T) {
	sb := NewBoard("scope-imm")
	sched := NewScheduler()
	k := New(context.Background(), sb, WithScheduler(sched), WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	sched.Start()
	defer k.Stop()

	tool := &SubmitTool{Kanban: k}
	result, err := tool.Execute(context.Background(),
		`{"target_agent_id":"builder","query":"immediate task"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var resp map[string]string
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "submitted" {
		t.Fatalf("expected status=submitted, got %q", resp["status"])
	}
	if resp["card_id"] == "" {
		t.Fatal("expected non-empty card_id for immediate submission")
	}
}

// Ensure Card type is wired for tools.
var _ = (*Card)(nil)
