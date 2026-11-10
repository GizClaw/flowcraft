package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/checkpoint"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

func newTaskStore(t *testing.T) *TaskStore {
	t.Helper()
	backend, err := checkpoint.Open(t.TempDir() + "/checkpoints.db")
	if err != nil {
		t.Fatalf("checkpoint.Open: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	store, err := NewTaskStore(backend, "sha256:test")
	if err != nil {
		t.Fatalf("NewTaskStore: %v", err)
	}
	return store
}

func a2aTask(id, contextID string) *a2a.Task {
	return &a2a.Task{
		ID:        a2a.TaskID(id),
		ContextID: contextID,
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
		History:   []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))},
	}
}

func TestTaskStoreRoundTrip(t *testing.T) {
	store := newTaskStore(t)
	ctx := context.Background()

	version, err := store.Create(ctx, a2aTask("task-1", "conv-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if version == taskstore.TaskVersionMissing {
		t.Error("version missing, want a tracked version")
	}
	stored, err := store.Get(ctx, "task-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Task.ContextID != "conv-1" || stored.Task.Status.State != a2a.TaskStateWorking {
		t.Errorf("task = %+v", stored.Task)
	}
	if len(stored.Task.History) != 1 {
		t.Errorf("history = %d, want the message the task was created with", len(stored.Task.History))
	}

	// The server creates a task before executing it and the executor's first
	// event carries the same task back, so re-creating the identical task is
	// idempotent. A different task under the same id is refused.
	again, err := store.Create(ctx, a2aTask("task-1", "conv-1"))
	if err != nil {
		t.Errorf("re-creating the same task: %v", err)
	}
	if again != version {
		t.Errorf("version = %d, want the existing %d", again, version)
	}
	if _, err := store.Create(ctx, a2aTask("task-1", "conv-2")); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Errorf("err = %v, want ErrTaskAlreadyExists", err)
	}
}

// Update appends the server's event; it never advances the projection, because
// the loop owns that.
func TestTaskStoreUpdateAppendsOnly(t *testing.T) {
	store := newTaskStore(t)
	ctx := context.Background()
	version, err := store.Create(ctx, a2aTask("task-1", "conv-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	update := &taskstore.UpdateRequest{
		Task:        a2aTask("task-1", "conv-1"),
		Event:       a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("again")),
		PrevVersion: version,
	}
	next, err := store.Update(ctx, update)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if next <= version {
		t.Errorf("version = %d, want it to advance past %d", next, version)
	}
	stored, err := store.Get(ctx, "task-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(stored.Task.History) != 2 {
		t.Errorf("history = %d, want the appended message", len(stored.Task.History))
	}
	// A stale writer is rejected rather than silently overwriting.
	stale := &taskstore.UpdateRequest{
		Task:        a2aTask("task-1", "conv-1"),
		Event:       a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("late")),
		PrevVersion: version,
	}
	if _, err := store.Update(ctx, stale); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Errorf("err = %v, want ErrConcurrentModification", err)
	}
}

func TestTaskStoreListFiltersByContext(t *testing.T) {
	store := newTaskStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if _, err := store.Create(ctx, a2aTask(id, "conv-1")); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if _, err := store.Create(ctx, a2aTask("c", "conv-2")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	response, err := store.List(ctx, &a2a.ListTasksRequest{ContextID: "conv-1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(response.Tasks) != 2 {
		t.Errorf("tasks = %d, want 2 for conv-1", len(response.Tasks))
	}
}

func TestTaskStoreReportsMissingTask(t *testing.T) {
	store := newTaskStore(t)
	if _, err := store.Get(context.Background(), "absent"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

var _ = agent.CheckpointSchema
var _ = message.RoleUser

// A status or artifact event is the engine's business: the adapter only moves its
// own token, and the projection it does not own stays untouched.
func TestNonTerminalUpdateLeavesTheProjectionAlone(t *testing.T) {
	adapter, store, _ := newStore(t)
	ctx := context.Background()
	seed := agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task: agent.Task{
			ID: "task-1", ContextID: "conv-1", State: agent.StateWorking, Round: 3,
		},
	}
	if _, _, err := store.Save(ctx, seed, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	update := &taskstore.UpdateRequest{
		Task: &a2a.Task{
			ID: "task-1", ContextID: "conv-1",
			Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		},
		Event: &a2a.TaskArtifactUpdateEvent{
			TaskID: "task-1", ContextID: "conv-1",
			Artifact: &a2a.Artifact{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("chunk")}},
		},
		PrevVersion: taskstore.TaskVersion(before.ServerVersion),
	}
	version, err := adapter.Update(ctx, update)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load after: %v", err)
	}
	if after.Version != before.Version {
		t.Errorf("version %d -> %d; the projection was rewritten", before.Version, after.Version)
	}
	if after.ServerVersion != before.ServerVersion+1 || int64(version) != after.ServerVersion {
		t.Errorf("server version = %d (returned %d), want %d",
			after.ServerVersion, version, before.ServerVersion+1)
	}
	if after.Task.State != before.Task.State || after.Task.Round != before.Task.Round {
		t.Errorf("the projection changed: %+v -> %+v", before.Task, after.Task)
	}
}

// A terminal state is the server's decision, so it is written even though the
// engine checkpointed the same task a moment earlier.
func TestTerminalUpdateWritesTheState(t *testing.T) {
	adapter, store, _ := newStore(t)
	ctx := context.Background()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task:   agent.Task{ID: "task-1", ContextID: "conv-1", State: agent.StateWorking},
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	current, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := adapter.Update(ctx, &taskstore.UpdateRequest{
		Task: &a2a.Task{
			ID: "task-1", ContextID: "conv-1",
			Status: a2a.TaskStatus{State: a2a.TaskStateCanceled},
		},
		PrevVersion: taskstore.TaskVersion(current.ServerVersion),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load after: %v", err)
	}
	if after.Task.State != agent.StateCanceled {
		t.Errorf("state = %s, want canceled", after.Task.State)
	}
}

func seedTask(t *testing.T, store agent.Store, id, conversationID string, state agent.TaskState) {
	t.Helper()
	if _, _, err := store.Save(context.Background(), agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task:   agent.Task{ID: id, ContextID: conversationID, State: state},
	}, nil); err != nil {
		t.Fatalf("Save %s: %v", id, err)
	}
}

// The protocol's list is a filtered page, not the whole table.
func TestListFiltersAndPaginates(t *testing.T) {
	adapter, store, _ := newStore(t)
	ctx := context.Background()
	seedTask(t, store, "t1", "conv-1", agent.StateCompleted)
	seedTask(t, store, "t2", "conv-1", agent.StateCompleted)
	seedTask(t, store, "t3", "conv-1", agent.StateWorking)
	seedTask(t, store, "t4", "conv-2", agent.StateFailed)
	seedTask(t, store, "t5", "conv-2", agent.StateCanceled)

	first, err := adapter.List(ctx, &a2a.ListTasksRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(first.Tasks) != 2 || first.TotalSize != 5 || first.PageSize != 2 {
		t.Fatalf("page = %d tasks (total %d, size %d), want 2 of 5 at size 2",
			len(first.Tasks), first.TotalSize, first.PageSize)
	}
	if first.NextPageToken == "" {
		t.Fatal("the first page has no token")
	}
	second, err := adapter.List(ctx, &a2a.ListTasksRequest{PageSize: 2, PageToken: first.NextPageToken})
	if err != nil {
		t.Fatalf("List 2: %v", err)
	}
	if len(second.Tasks) != 2 || second.NextPageToken == "" {
		t.Fatalf("second page = %d tasks, token %q", len(second.Tasks), second.NextPageToken)
	}
	last, err := adapter.List(ctx, &a2a.ListTasksRequest{PageSize: 2, PageToken: second.NextPageToken})
	if err != nil {
		t.Fatalf("List 3: %v", err)
	}
	if len(last.Tasks) != 1 || last.NextPageToken != "" {
		t.Fatalf("last page = %d tasks, token %q", len(last.Tasks), last.NextPageToken)
	}
	byContext, err := adapter.List(ctx, &a2a.ListTasksRequest{ContextID: "conv-1"})
	if err != nil {
		t.Fatalf("List context: %v", err)
	}
	if len(byContext.Tasks) != 3 || byContext.TotalSize != 3 {
		t.Errorf("context filter = %d tasks (total %d), want 3", len(byContext.Tasks), byContext.TotalSize)
	}
	byStatus, err := adapter.List(ctx, &a2a.ListTasksRequest{Status: a2a.TaskStateCompleted})
	if err != nil {
		t.Fatalf("List status: %v", err)
	}
	if len(byStatus.Tasks) != 2 {
		t.Errorf("status filter = %d tasks, want 2", len(byStatus.Tasks))
	}
	if _, err := adapter.List(ctx, &a2a.ListTasksRequest{PageToken: "nonsense"}); err == nil {
		t.Error("an invalid page token was accepted")
	}
}

// HistoryLength bounds the history each listed task carries.
func TestListHistoryLength(t *testing.T) {
	adapter, store, _ := newStore(t)
	ctx := context.Background()
	events := make([]agent.EventRecord, 0, 3)
	for _, text := range []string{"one", "two", "three"} {
		body, err := json.Marshal(message.NewTextMessage(message.RoleUser, text))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		events = append(events, agent.EventRecord{Kind: "message", Payload: body})
	}
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task:   agent.Task{ID: "t1", ContextID: "conv-1", State: agent.StateCompleted},
	}, events); err != nil {
		t.Fatalf("Save: %v", err)
	}
	limit := 2
	response, err := adapter.List(ctx, &a2a.ListTasksRequest{HistoryLength: &limit})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(response.Tasks) != 1 {
		t.Fatalf("tasks = %d, want one", len(response.Tasks))
	}
	if history := response.Tasks[0].History; len(history) != 2 {
		t.Fatalf("history = %d messages, want the last 2", len(history))
	}
}
