package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	sdkmodel "github.com/GizClaw/flowcraft/sdk/model"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSQLiteStore(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

func TestAgentCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Create
	agent := &model.Agent{Name: "Test Agent", Type: model.AgentTypeWorkflow, Description: "desc"}
	created, err := s.CreateAgent(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if created.AgentID == "" {
		t.Fatal("expected auto-generated ID")
	}

	// Get
	got, err := s.GetAgent(ctx, created.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Test Agent" {
		t.Fatalf("expected 'Test Agent', got %q", got.Name)
	}

	// Update
	got.Name = "Updated Agent"
	updated, err := s.UpdateAgent(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Updated Agent" {
		t.Fatalf("expected 'Updated Agent', got %q", updated.Name)
	}

	// List
	agents, lr, err := s.ListAgents(ctx, model.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if lr.HasMore {
		t.Fatal("expected no more pages")
	}

	// Delete
	err = s.DeleteAgent(ctx, created.AgentID)
	if err != nil {
		t.Fatal(err)
	}

	// Verify deleted
	_, err = s.GetAgent(ctx, created.AgentID)
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestDeleteAgent_CoPilotBlocked(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agent, _ := s.CreateAgent(ctx, &model.Agent{Name: "CoPilot", Type: model.AgentTypeCoPilot})
	err := s.DeleteAgent(ctx, agent.AgentID)
	if err == nil {
		t.Fatal("expected error deleting copilot agent")
	}
}

func TestConversationCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agent, _ := s.CreateAgent(ctx, &model.Agent{Name: "Agent", Type: model.AgentTypeWorkflow})

	conv, err := s.CreateConversation(ctx, &model.Conversation{AgentID: agent.AgentID, RuntimeID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if conv.Status != model.ConvActive {
		t.Fatalf("expected 'active', got %q", conv.Status)
	}

	got, err := s.GetConversation(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RuntimeID != "owner" {
		t.Fatalf("expected 'owner', got %q", got.RuntimeID)
	}

	convs, _, err := s.ListConversations(ctx, agent.AgentID, model.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		t.Fatalf("expected 1 conversation, got %d", len(convs))
	}
}

func TestMessageCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agent, _ := s.CreateAgent(ctx, &model.Agent{Name: "Agent", Type: model.AgentTypeWorkflow})
	conv, _ := s.CreateConversation(ctx, &model.Conversation{AgentID: agent.AgentID})

	for i := 0; i < 5; i++ {
		err := s.SaveMessage(ctx, &model.Message{
			Message:        sdkmodel.NewTextMessage(model.RoleUser, "msg"),
			ConversationID: conv.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.GetMessages(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(all))
	}

	recent, err := s.GetRecentMessages(ctx, conv.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 3 {
		t.Fatalf("expected 3 recent messages, got %d", len(recent))
	}
}

func TestWorkflowRunCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agent, _ := s.CreateAgent(ctx, &model.Agent{Name: "Agent", Type: model.AgentTypeWorkflow})

	run := &model.WorkflowRun{
		AgentID: agent.AgentID,
		Input:   "hello",
		Status:  "running",
	}
	err := s.SaveWorkflowRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("expected 'running', got %q", got.Status)
	}

	// Update via upsert
	run.Status = "completed"
	run.Output = "world"
	err = s.SaveWorkflowRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}

	got, _ = s.GetWorkflowRun(ctx, run.ID)
	if got.Status != "completed" {
		t.Fatalf("expected 'completed', got %q", got.Status)
	}
}

func TestGraphVersionPublish(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agent, _ := s.CreateAgent(ctx, &model.Agent{Name: "Agent", Type: model.AgentTypeWorkflow})

	def := &model.GraphDefinition{Name: "test", Entry: "start", Nodes: []model.NodeDefinition{{ID: "start", Type: "llm"}}}

	v1, err := s.PublishGraphVersion(ctx, agent.AgentID, def, "first version")
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 {
		t.Fatalf("expected version 1, got %d", v1.Version)
	}

	v2, err := s.PublishGraphVersion(ctx, agent.AgentID, def, "second version")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 {
		t.Fatalf("expected version 2, got %d", v2.Version)
	}

	latest, err := s.GetLatestPublishedVersion(ctx, agent.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 2 {
		t.Fatalf("expected latest version 2, got %d", latest.Version)
	}
}

func TestProviderConfig(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	pc := &model.ProviderConfig{
		Provider: "openai",
		Config:   map[string]any{"api_key": "sk-test"},
	}
	err := s.SetProviderConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetProviderConfig(ctx, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if got.Config["api_key"] != "sk-test" {
		t.Fatalf("expected 'sk-test', got %v", got.Config["api_key"])
	}

	// Update
	pc.Config["api_key"] = "sk-updated"
	err = s.SetProviderConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}

	got, _ = s.GetProviderConfig(ctx, "openai")
	if got.Config["api_key"] != "sk-updated" {
		t.Fatalf("expected 'sk-updated', got %v", got.Config["api_key"])
	}

	all, err := s.ListProviderConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 provider config, got %d", len(all))
	}
}

func TestStats(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agent, err := s.CreateAgent(ctx, &model.Agent{Name: "Agent", Type: model.AgentTypeWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConversation(ctx, &model.Conversation{AgentID: agent.AgentID}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWorkflowRun(ctx, &model.WorkflowRun{AgentID: agent.AgentID, Status: "completed", ElapsedMs: 100}); err != nil {
		t.Fatal(err)
	}

	stats, err := s.GetStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalAgents != 1 || stats.TotalConversations != 1 || stats.TotalRuns != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	rs, err := s.GetRunStats(ctx, agent.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if rs.CompletedRuns != 1 {
		t.Fatalf("expected 1 completed run, got %d", rs.CompletedRuns)
	}
}

func TestMonitoringDiagnostics_UsesStructuredErrorCode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	agent, _ := s.CreateAgent(ctx, &model.Agent{Name: "Agent", Type: model.AgentTypeWorkflow})

	run1 := &model.WorkflowRun{
		AgentID:   agent.AgentID,
		Status:    "failed",
		Output:    "request failed",
		Outputs:   map[string]any{"error_code": "RATE_LIMIT"},
		ElapsedMs: 1200,
		CreatedAt: time.Now().UTC().Add(-2 * time.Minute),
	}
	if err := s.SaveWorkflowRun(ctx, run1); err != nil {
		t.Fatal(err)
	}

	run2 := &model.WorkflowRun{
		AgentID:   agent.AgentID,
		Status:    "failed",
		Output:    "provider call failed",
		ElapsedMs: 800,
		CreatedAt: time.Now().UTC().Add(-1 * time.Minute),
	}
	if err := s.SaveWorkflowRun(ctx, run2); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveExecutionEvent(ctx, &model.ExecutionEvent{
		RunID: run2.ID,
		Type:  "node.error",
		Payload: map[string]any{
			"error": map[string]any{
				"code": "MODEL_TIMEOUT",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	diag, err := s.GetMonitoringDiagnostics(ctx, agent.AgentID, time.Now().UTC().Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(diag.RecentFailures) < 2 {
		t.Fatalf("expected at least 2 recent failures, got %d", len(diag.RecentFailures))
	}
	gotCodes := map[string]bool{}
	for _, item := range diag.TopErrorCodes {
		gotCodes[item.Code] = true
	}
	if !gotCodes["rate_limit"] {
		t.Fatalf("expected structured code rate_limit in top codes, got %+v", diag.TopErrorCodes)
	}
	if !gotCodes["model_timeout"] {
		t.Fatalf("expected event-derived code model_timeout in top codes, got %+v", diag.TopErrorCodes)
	}
}

func TestMonitoringSummary_EmptyWindowReturnsNilRates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	agent, _ := s.CreateAgent(ctx, &model.Agent{Name: "Agent", Type: model.AgentTypeWorkflow})

	summary, err := s.GetMonitoringSummary(ctx, agent.AgentID, time.Now().UTC().Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if summary.RunTotal != 0 {
		t.Fatalf("expected run total 0, got %d", summary.RunTotal)
	}
	if summary.SuccessRate != nil || summary.ErrorRate != nil {
		t.Fatalf("expected nil rates on empty window, got success=%v error=%v", summary.SuccessRate, summary.ErrorRate)
	}
	if summary.LatencyP95Ms != nil || summary.LatencyP99Ms != nil {
		t.Fatalf("expected nil latency percentiles on empty window, got p95=%v p99=%v", summary.LatencyP95Ms, summary.LatencyP99Ms)
	}
}

func TestMonitoringTimeseries_BucketAggregation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	agent, _ := s.CreateAgent(ctx, &model.Agent{Name: "Agent", Type: model.AgentTypeWorkflow})
	now := time.Now().UTC()

	// Anchor both runs to the same 5-minute bucket by using the bucket
	// start as base, then offsetting by 1 and 2 minutes within the bucket.
	bucketStart := now.Add(-15 * time.Minute).Truncate(5 * time.Minute)
	run1 := &model.WorkflowRun{
		AgentID:   agent.AgentID,
		Status:    "completed",
		ElapsedMs: 1000,
		CreatedAt: bucketStart.Add(1 * time.Minute),
	}
	run2 := &model.WorkflowRun{
		AgentID:   agent.AgentID,
		Status:    "failed",
		ElapsedMs: 3000,
		CreatedAt: bucketStart.Add(2 * time.Minute),
	}
	if err := s.SaveWorkflowRun(ctx, run1); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWorkflowRun(ctx, run2); err != nil {
		t.Fatal(err)
	}

	points, err := s.ListMonitoringTimeseries(ctx, agent.AgentID, now.Add(-20*time.Minute), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) == 0 {
		t.Fatal("expected at least one timeseries bucket")
	}
	var matched *model.MonitoringTimeseriesPoint
	for _, p := range points {
		if p.RunTotal == 2 {
			matched = p
			break
		}
	}
	if matched == nil {
		t.Fatalf("expected one bucket with run_total=2, got %+v", points)
	}
	if matched.RunSuccess != 1 || matched.RunFailed != 1 {
		t.Fatalf("unexpected success/failed in bucket: %+v", matched)
	}
	if matched.SuccessRate == nil || *matched.SuccessRate != 0.5 {
		t.Fatalf("expected success rate 0.5, got %v", matched.SuccessRate)
	}
}

func TestCascadeDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agent, err := s.CreateAgent(ctx, &model.Agent{Name: "Agent", Type: model.AgentTypeWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.CreateConversation(ctx, &model.Conversation{AgentID: agent.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMessage(ctx, &model.Message{Message: sdkmodel.NewTextMessage(model.RoleUser, "hi"), ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	run := &model.WorkflowRun{AgentID: agent.AgentID, Status: "completed"}
	if err := s.SaveWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveExecutionEvent(ctx, &model.ExecutionEvent{RunID: run.ID, Type: "node.start"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishGraphVersion(ctx, agent.AgentID, &model.GraphDefinition{Name: "g", Entry: "s", Nodes: []model.NodeDefinition{{ID: "s", Type: "t"}}}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAgent(ctx, agent.AgentID); err != nil {
		t.Fatal(err)
	}

	// Verify everything is gone
	msgs, _ := s.GetMessages(ctx, conv.ID)
	if len(msgs) != 0 {
		t.Fatal("expected messages to be deleted")
	}
	events, _ := s.ListExecutionEvents(ctx, run.ID)
	if len(events) != 0 {
		t.Fatal("expected events to be deleted")
	}
}

func TestDeleteAgent_CascadeDeletesAllRelated(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agent, err := s.CreateAgent(ctx, &model.Agent{Name: "Full Agent", Type: model.AgentTypeWorkflow})
	if err != nil {
		t.Fatal(err)
	}

	// Create related entities
	conv, err := s.CreateConversation(ctx, &model.Conversation{AgentID: agent.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMessage(ctx, &model.Message{Message: sdkmodel.NewTextMessage(model.RoleUser, "hi"), ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMessage(ctx, &model.Message{Message: sdkmodel.NewTextMessage(model.RoleAssistant, "hello"), ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	run := &model.WorkflowRun{AgentID: agent.AgentID, Status: "completed"}
	if err := s.SaveWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveExecutionEvent(ctx, &model.ExecutionEvent{RunID: run.ID, Type: "node.start"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveExecutionEvent(ctx, &model.ExecutionEvent{RunID: run.ID, Type: "node.end"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishGraphVersion(ctx, agent.AgentID, &model.GraphDefinition{
		Name: "g", Entry: "s", Nodes: []model.NodeDefinition{{ID: "s", Type: "t"}},
	}, "v1"); err != nil {
		t.Fatal(err)
	}
	// Create a dataset with documents
	ds, err := s.CreateDataset(ctx, &model.Dataset{Name: "ds", AgentID: agent.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDocument(ctx, ds.ID, "doc1", "content1"); err != nil {
		t.Fatal(err)
	}

	// Delete agent — should cascade
	if err := s.DeleteAgent(ctx, agent.AgentID); err != nil {
		t.Fatalf("DeleteAgent failed: %v", err)
	}

	// Verify everything is gone
	msgs, _ := s.GetMessages(ctx, conv.ID)
	if len(msgs) != 0 {
		t.Fatalf("expected messages deleted, got %d", len(msgs))
	}
	events, _ := s.ListExecutionEvents(ctx, run.ID)
	if len(events) != 0 {
		t.Fatalf("expected events deleted, got %d", len(events))
	}
	versions, _ := s.ListGraphVersions(ctx, agent.AgentID)
	if len(versions) != 0 {
		t.Fatalf("expected versions deleted, got %d", len(versions))
	}
	docs, _ := s.ListDocuments(ctx, ds.ID)
	if len(docs) != 0 {
		t.Fatalf("expected documents deleted, got %d", len(docs))
	}
}

func TestKanbanCardCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	card := &model.KanbanCard{
		KanbanCardModel: kanban.KanbanCardModel{
			ID:            "card-1",
			RuntimeID:     "owner",
			Type:          "task",
			Status:        "pending",
			Producer:      "copilot",
			Consumer:      "*",
			TargetAgentID: "coder",
			Query:         "fix the bug",
		},
	}
	if err := s.SaveKanbanCard(ctx, card); err != nil {
		t.Fatal(err)
	}

	// List
	cards, err := s.ListKanbanCards(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if cards[0].Query != "fix the bug" {
		t.Fatalf("expected query 'fix the bug', got %q", cards[0].Query)
	}
	if cards[0].TargetAgentID != "coder" {
		t.Fatalf("expected target_agent_id 'coder', got %q", cards[0].TargetAgentID)
	}

	// UPSERT: update status and output
	card.Status = "done"
	card.Output = "bug fixed"
	card.RunID = "run-123"
	if err := s.SaveKanbanCard(ctx, card); err != nil {
		t.Fatal(err)
	}

	cards, _ = s.ListKanbanCards(ctx, "owner")
	if cards[0].Status != "done" {
		t.Fatalf("expected status 'done', got %q", cards[0].Status)
	}
	if cards[0].Output != "bug fixed" {
		t.Fatalf("expected output 'bug fixed', got %q", cards[0].Output)
	}
	if cards[0].RunID != "run-123" {
		t.Fatalf("expected run_id 'run-123', got %q", cards[0].RunID)
	}

	// List for different user returns empty
	other, _ := s.ListKanbanCards(ctx, "other-runtime")
	if len(other) != 0 {
		t.Fatalf("expected 0 cards for other runtime, got %d", len(other))
	}

	// Delete
	if err := s.DeleteKanbanCards(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	cards, _ = s.ListKanbanCards(ctx, "owner")
	if len(cards) != 0 {
		t.Fatalf("expected 0 cards after delete, got %d", len(cards))
	}
}

func TestKanbanCard_MetaPersistence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	card := &model.KanbanCard{
		KanbanCardModel: kanban.KanbanCardModel{
			ID:        "card-meta",
			RuntimeID: "owner-meta",
			Type:      "task",
			Status:    "pending",
			Producer:  "copilot",
		},
		Meta: map[string]any{"priority": "high"},
	}
	if err := s.SaveKanbanCard(ctx, card); err != nil {
		t.Fatal(err)
	}

	cards, _ := s.ListKanbanCards(ctx, "owner-meta")
	if cards[0].Meta["priority"] != "high" {
		t.Fatalf("expected meta priority=high, got %v", cards[0].Meta)
	}
}

func TestKanbanCard_MultipleCards_Ordering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for i, id := range []string{"c3", "c1", "c2"} {
		_ = s.SaveKanbanCard(ctx, &model.KanbanCard{
			KanbanCardModel: kanban.KanbanCardModel{
				ID:        id,
				RuntimeID: "owner-order",
				Type:      "task",
				Status:    "pending",
				Producer:  "copilot",
				CreatedAt: time.Now().Add(time.Duration(i) * time.Second),
			},
		})
	}

	cards, _ := s.ListKanbanCards(ctx, "owner-order")
	if len(cards) != 3 {
		t.Fatalf("expected 3 cards, got %d", len(cards))
	}
	// Ordered by created_at ASC
	if cards[0].ID != "c3" || cards[1].ID != "c1" || cards[2].ID != "c2" {
		t.Fatalf("unexpected order: %s, %s, %s", cards[0].ID, cards[1].ID, cards[2].ID)
	}
}

func TestTemplateCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tmpl := &model.Template{
		Name:        "my_flow",
		Label:       "My Flow",
		Description: "a custom flow",
		Category:    "custom",
		Parameters:  `[{"name":"prompt","type":"string"}]`,
		GraphDef:    `{"entry":"start","nodes":[]}`,
		IsBuiltin:   false,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := s.SaveTemplate(ctx, tmpl); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 template, got %d", len(all))
	}
	if all[0].Label != "My Flow" {
		t.Fatalf("Label = %q, want %q", all[0].Label, "My Flow")
	}

	// Upsert
	tmpl.Label = "Updated Flow"
	tmpl.UpdatedAt = time.Now().UTC()
	if err := s.SaveTemplate(ctx, tmpl); err != nil {
		t.Fatal(err)
	}
	all, _ = s.ListTemplates(ctx)
	if all[0].Label != "Updated Flow" {
		t.Fatalf("Label after upsert = %q, want %q", all[0].Label, "Updated Flow")
	}

	// Delete
	if err := s.DeleteTemplate(ctx, "my_flow"); err != nil {
		t.Fatal(err)
	}
	all, _ = s.ListTemplates(ctx)
	if len(all) != 0 {
		t.Fatalf("expected 0 templates after delete, got %d", len(all))
	}
}

func TestDeleteTemplate_BuiltinBlocked(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tmpl := &model.Template{
		Name:      "builtin_flow",
		Label:     "Built-in",
		GraphDef:  `{"entry":"start"}`,
		IsBuiltin: true,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	_ = s.SaveTemplate(ctx, tmpl)

	err := s.DeleteTemplate(ctx, "builtin_flow")
	if err == nil {
		t.Fatal("expected error deleting built-in template")
	}

	all, _ := s.ListTemplates(ctx)
	if len(all) != 1 {
		t.Fatal("built-in template should still exist after failed delete")
	}
}

func TestDeleteTemplate_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.DeleteTemplate(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error deleting nonexistent template")
	}
}

func TestOwnerCredential_SetAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetOwnerCredential(ctx)
	if err == nil {
		t.Fatal("expected not-found error before setup")
	}

	cred := &model.OwnerCredential{
		Username:     "admin",
		PasswordHash: "$2a$10$fakehashfakehashfakehashfakehashfakehashfakehashfake",
	}
	if err := s.SetOwnerCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetOwnerCredential(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "admin" {
		t.Fatalf("username = %q, want admin", got.Username)
	}
	if got.PasswordHash != cred.PasswordHash {
		t.Fatalf("password_hash mismatch")
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at should be set")
	}

	cred2 := &model.OwnerCredential{
		Username:     "admin",
		PasswordHash: "$2a$10$updatedupdatedupdatedupdatedupdatedupdatedupdated",
	}
	if err := s.SetOwnerCredential(ctx, cred2); err != nil {
		t.Fatal(err)
	}
	got2, err := s.GetOwnerCredential(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got2.PasswordHash != cred2.PasswordHash {
		t.Fatal("password_hash should be updated after upsert")
	}
}

func TestSettings_SetAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetSetting(ctx, "jwt_secret")
	if err == nil {
		t.Fatal("expected not-found error for missing setting")
	}

	if err := s.SetSetting(ctx, "jwt_secret", "dGVzdC1zZWNyZXQ="); err != nil {
		t.Fatal(err)
	}
	val, err := s.GetSetting(ctx, "jwt_secret")
	if err != nil {
		t.Fatal(err)
	}
	if val != "dGVzdC1zZWNyZXQ=" {
		t.Fatalf("got %q, want dGVzdC1zZWNyZXQ=", val)
	}

	if err := s.SetSetting(ctx, "jwt_secret", "bmV3LXNlY3JldA=="); err != nil {
		t.Fatal(err)
	}
	val2, err := s.GetSetting(ctx, "jwt_secret")
	if err != nil {
		t.Fatal(err)
	}
	if val2 != "bmV3LXNlY3JldA==" {
		t.Fatalf("got %q after update", val2)
	}
}

func TestDBFilePermissions(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "perm-test.db")
	s, err := NewSQLiteStore(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Fatalf("db file permission = %04o, want 0600", perm)
	}
}

func TestMigration003_TablesExist(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='owner_credential'`).Scan(&name)
	if err != nil {
		t.Fatalf("owner_credential table missing: %v", err)
	}
	err = s.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='settings'`).Scan(&name)
	if err != nil {
		t.Fatalf("settings table missing: %v", err)
	}
}
