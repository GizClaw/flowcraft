package a2aprotocol

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

// openStore opens a checkpoint store and the adapter over it.
func openStore(t testing.TB) (*taskStore, *checkpoint.SQLite) {
	t.Helper()
	backend, err := checkpoint.Open(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatalf("checkpoint.Open: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return newTaskStore(backend, "sha256:deployment"), backend
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
	store, _ := openStore(t)
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
	store, _ := openStore(t)
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
	store, _ := openStore(t)
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
	store, _ := openStore(t)
	if _, err := store.Get(context.Background(), "absent"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

var _ = agent.CheckpointSchema
var _ = message.RoleUser

// A status or artifact event is the engine's business: the adapter only moves its
// own token, and the projection it does not own stays untouched.
func TestNonTerminalUpdateLeavesTheProjectionAlone(t *testing.T) {
	adapter, store := openStore(t)
	ctx := context.Background()
	seed := agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task: agent.Task{
			ID: "task-1", SessionID: "conv-1", State: agent.StateWorking, Round: 3,
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
	if after.ServerVersion != before.ServerVersion {
		t.Errorf("server version %d -> %d; a chunk committed the token",
			before.ServerVersion, after.ServerVersion)
	}
	if int64(version) != before.ServerVersion+1 {
		t.Errorf("returned version = %d, want %d", version, before.ServerVersion+1)
	}
	stored, err := adapter.Get(ctx, "task-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if int64(stored.Version) != int64(version) {
		t.Errorf("Get version = %d, want the handed-out %d", stored.Version, version)
	}
	if after.Task.State != before.Task.State || after.Task.Round != before.Task.Round {
		t.Errorf("the projection changed: %+v -> %+v", before.Task, after.Task)
	}
}

// A terminal state is the server's decision, so it is written even though the
// engine checkpointed the same task a moment earlier.
func TestTerminalUpdateWritesTheState(t *testing.T) {
	adapter, store := openStore(t)
	ctx := context.Background()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task:   agent.Task{ID: "task-1", SessionID: "conv-1", State: agent.StateWorking},
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

func seedTask(t *testing.T, store agent.Store, id, sessionID string, state agent.TaskState) {
	t.Helper()
	if _, _, err := store.Save(context.Background(), agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task:   agent.Task{ID: id, SessionID: sessionID, State: state},
	}, nil); err != nil {
		t.Fatalf("Save %s: %v", id, err)
	}
}

// The protocol's list is a filtered page, not the whole table.
func TestListFiltersAndPaginates(t *testing.T) {
	adapter, store := openStore(t)
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
	adapter, store := openStore(t)
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
		Task:   agent.Task{ID: "t1", SessionID: "conv-1", State: agent.StateCompleted},
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
	store, backend := openStore(t)
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
		Schema: agent.CheckpointSchema, Fingerprint: "sha256:deployment",
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

// Create stamps the deployment fingerprint, or a restored session could never
// pass agent.Restore.
func TestTaskStoreCreateStampsTheFingerprint(t *testing.T) {
	store, backend := openStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, taskWith("hi")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	checkpoint, err := backend.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if checkpoint.Fingerprint != "sha256:deployment" {
		t.Errorf("fingerprint = %q, want the deployment's", checkpoint.Fingerprint)
	}
	if checkpoint.ServerVersion == 0 {
		t.Error("the adapter's own version was not initialised")
	}
}

// The adapter's counter and the loop's counter are separate: a round-boundary
// save must not invalidate the token the server is holding.
func TestTaskStoreVersionIsIndependentOfTheLoops(t *testing.T) {
	store, backend := openStore(t)
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

// Chunk tokens stay in memory until a write flushes them: a streamed answer
// must not pay one commit per chunk, and a restart must not lose task state.
func TestTokenAdvancesCoalesceUntilAWrite(t *testing.T) {
	adapter, store := openStore(t)
	ctx := context.Background()
	if _, err := adapter.Create(ctx, a2aTask("task-1", "conv-1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	persisted, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	update := &taskstore.UpdateRequest{
		Task: &a2a.Task{
			ID: "task-1", ContextID: "conv-1",
			Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		},
	}
	version := taskstore.TaskVersion(persisted.ServerVersion)
	for i := 0; i < 3; i++ {
		update.PrevVersion = version
		version, err = adapter.Update(ctx, update)
		if err != nil {
			t.Fatalf("Update %d: %v", i, err)
		}
	}
	after, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load after: %v", err)
	}
	if after.ServerVersion != persisted.ServerVersion {
		t.Fatalf("server version = %d, want the persisted %d",
			after.ServerVersion, persisted.ServerVersion)
	}
	stored, err := adapter.Get(ctx, "task-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Version != version {
		t.Errorf("Get version = %d, want the handed-out %d", stored.Version, version)
	}

	// A terminal state is a write, and it carries the coalesced token.
	update.Task.Status = a2a.TaskStatus{State: a2a.TaskStateCanceled}
	update.PrevVersion = version
	flushed, err := adapter.Update(ctx, update)
	if err != nil {
		t.Fatalf("terminal Update: %v", err)
	}
	after, err = store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load flushed: %v", err)
	}
	if after.ServerVersion != int64(flushed) || after.Task.State != agent.StateCanceled {
		t.Errorf("flushed = %d/%s, want %d/canceled",
			after.ServerVersion, after.Task.State, flushed)
	}

	// The loop's own save carries no token and must not reset it.
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema: agent.CheckpointSchema,
		Task:   after.Task,
	}, nil); err != nil {
		t.Fatalf("loop Save: %v", err)
	}
	again, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load again: %v", err)
	}
	if again.ServerVersion != int64(flushed) {
		t.Errorf("server version = %d after a loop save, want %d",
			again.ServerVersion, flushed)
	}
}
