package server

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/checkpoint"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

func newStore(t *testing.T) (*TaskStore, agent.Store, *checkpoint.SQLite) {
	t.Helper()
	backend, err := checkpoint.Open(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatalf("checkpoint.Open: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	store, err := NewTaskStore(backend, "sha256:deployment")
	if err != nil {
		t.Fatalf("NewTaskStore: %v", err)
	}
	return store, backend, backend
}

func taskWith(text string) *a2a.Task {
	return &a2a.Task{
		ID: "task-1", ContextID: "conv-1",
		Status:  a2a.TaskStatus{State: a2a.TaskStateSubmitted},
		History: []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))},
	}
}

// The loop persists its own working (tool calls, results) in the same log;
// tasks/get must skip those rather than fail the whole task.
func TestTaskStoreSkipsInternalEvents(t *testing.T) {
	store, backend, _ := newStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, taskWith("hi")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	call, err := message.NewToolCall("call-1", "read_file", map[string]any{"file_path": "/x"})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	// What the loop writes at a round boundary: an assistant message carrying a
	// tool call, and a tool observation.
	assistant := message.Message{Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: call}}}}
	observation := message.Message{Role: message.RoleTool,
		Content: message.Content{Parts: []message.Part{message.ToolResultPart{
			Result: message.NewTextToolResult("call-1", "ok")}}}}
	events := make([]agent.EventRecord, 0, 2)
	for _, msg := range []message.Message{assistant, observation} {
		body, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		events = append(events, agent.EventRecord{Kind: "message", Payload: body})
	}
	if _, _, err := backend.Save(ctx, agent.Checkpoint{
		Schema: agent.CheckpointSchema, Runtime: "sha256:deployment",
		ServerVersion: 1, Task: agent.Task{ID: "task-1", SessionID: "conv-1", State: agent.StateWorking},
	}, events); err != nil {
		t.Fatalf("Save: %v", err)
	}
	stored, err := store.Get(ctx, "task-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(stored.Task.History) != 1 {
		t.Errorf("history = %d, want only the wire-representable message", len(stored.Task.History))
	}
}

// Create stamps the deployment digest, or a restored session could never
// pass agent.Restore.
func TestTaskStoreCreateStampsTheDigest(t *testing.T) {
	store, backend, _ := newStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, taskWith("hi")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	checkpoint, err := backend.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if checkpoint.Runtime != "sha256:deployment" {
		t.Errorf("digest = %q, want the deployment's", checkpoint.Runtime)
	}
	if checkpoint.ServerVersion == 0 {
		t.Error("the adapter's own version was not initialised")
	}
}

// The adapter's counter and the loop's counter are separate: a round-boundary
// save must not invalidate the token the server is holding.
func TestTaskStoreVersionIsIndependentOfTheLoops(t *testing.T) {
	store, backend, _ := newStore(t)
	ctx := context.Background()
	version, err := store.Create(ctx, taskWith("hi"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	update := &taskstore.UpdateRequest{
		Task:        taskWith("again"),
		Event:       a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("again")),
		PrevVersion: version,
	}
	next, err := store.Update(ctx, update)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	// The loop persists at a round boundary, bumping its own counter.
	current, err := backend.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, _, err := backend.Save(ctx, current, nil); err != nil {
		t.Fatalf("loop Save: %v", err)
	}

	// The server's next event still sees its own version.
	update.PrevVersion = next
	if _, err := store.Update(ctx, update); err != nil {
		t.Fatalf("Update after the loop's save: %v", err)
	}
	// A stale token is refused.
	update.PrevVersion = version
	if _, err := store.Update(ctx, update); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Errorf("err = %v, want ErrConcurrentModification", err)
	}
}
