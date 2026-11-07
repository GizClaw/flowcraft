package a2a

import (
	"context"
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
	store, err := NewTaskStore(backend)
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

	// A second create of the same id is what the server uses to detect a
	// duplicate, so it must be refused.
	if _, err := store.Create(ctx, a2aTask("task-1", "conv-1")); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
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
