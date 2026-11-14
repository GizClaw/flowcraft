package a2aprotocol

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// BenchmarkTextStream is one answer delta: the chunk that was held back is
// emitted and the new delta takes its place.
func BenchmarkTextStream(b *testing.B) {
	stream := newTextStream(testInfo(), nil)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		stream.events(agent.Text{Text: "a delta"})
	}
}

// BenchmarkDecodeSettings is a deployment load: the card block is decoded and
// validated once per deployment.
func BenchmarkDecodeSettings(b *testing.B) {
	skills := make([]any, 5)
	for i := range skills {
		skills[i] = map[string]any{"id": fmt.Sprintf("skill-%d", i), "name": "Skill"}
	}
	settings := settingsJSON(map[string]any{"card": map[string]any{
		"version": "1.0.0",
		"url":     "https://example.test",
		"skills":  skills,
	}})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := DecodeSettings(settings); err != nil {
			b.Fatalf("DecodeSettings: %v", err)
		}
	}
}

// BenchmarkTaskStoreCreate is the first write of a task: convert it, copy its
// history into the event log, and save.
func BenchmarkTaskStoreCreate(b *testing.B) {
	store := newTaskStore(agenttest.NewMemStore(), "sha256:deployment")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if _, err := store.Create(ctx, a2aTask(fmt.Sprintf("task-%d", i), "conv-1")); err != nil {
			b.Fatalf("Create: %v", err)
		}
	}
}

// BenchmarkTaskStoreUpdateToken is a non-terminal event with no message: the
// read-model path advances the server token and leaves the projection alone.
func BenchmarkTaskStoreUpdateToken(b *testing.B) {
	store := newTaskStore(agenttest.NewMemStore(), "sha256:deployment")
	ctx := context.Background()
	if _, err := store.Create(ctx, a2aTask("task-1", "conv-1")); err != nil {
		b.Fatalf("Create: %v", err)
	}
	update := &taskstore.UpdateRequest{
		Task: &a2a.Task{
			ID: "task-1", ContextID: "conv-1",
			Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	version := taskstore.TaskVersion(1)
	for b.Loop() {
		update.PrevVersion = version
		next, err := store.Update(ctx, update)
		if err != nil {
			b.Fatalf("Update: %v", err)
		}
		version = next
	}
}

// BenchmarkTaskStoreUpdateTokenSQLite is the coalesced token advance: no store
// write per chunk, so it pays the adapter's bookkeeping and nothing else.
func BenchmarkTaskStoreUpdateTokenSQLite(b *testing.B) {
	store, _ := openStore(b)
	ctx := context.Background()
	if _, err := store.Create(ctx, a2aTask("task-1", "conv-1")); err != nil {
		b.Fatalf("Create: %v", err)
	}
	update := &taskstore.UpdateRequest{
		Task: &a2a.Task{
			ID: "task-1", ContextID: "conv-1",
			Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	version := taskstore.TaskVersion(1)
	for b.Loop() {
		update.PrevVersion = version
		next, err := store.Update(ctx, update)
		if err != nil {
			b.Fatalf("Update: %v", err)
		}
		version = next
	}
}

// BenchmarkTaskStoreGet is a tasks/get: load the projection and append the
// event log.
func BenchmarkTaskStoreGet(b *testing.B) {
	store := newTaskStore(agenttest.NewMemStore(), "sha256:deployment")
	ctx := context.Background()
	if _, err := store.Create(ctx, a2aTask("task-1", "conv-1")); err != nil {
		b.Fatalf("Create: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := store.Get(ctx, "task-1"); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

// BenchmarkTaskStoreList is a filtered page over a thousand tasks.
func BenchmarkTaskStoreList(b *testing.B) {
	store := newTaskStore(agenttest.NewMemStore(), "sha256:deployment")
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		if _, err := store.Create(ctx, a2aTask(fmt.Sprintf("task-%d", i), "conv-1")); err != nil {
			b.Fatalf("Create: %v", err)
		}
	}
	request := &a2a.ListTasksRequest{PageSize: 50}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := store.List(ctx, request); err != nil {
			b.Fatalf("List: %v", err)
		}
	}
}

// BenchmarkExecutorExecute is one A2A execution end to end: translate the
// message, drive the session table, translate the events back.
func BenchmarkExecutorExecute(b *testing.B) {
	executor, err := newExecutor(executorConfig{
		Card:  cardConfig{Name: "x", Version: "1", URL: "https://x"},
		Agent: tableFor(b, &completedRunner{}),
	})
	if err != nil {
		b.Fatalf("newExecutor: %v", err)
	}
	exec := execContext()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, err := range executor.Execute(context.Background(), exec) {
			if err != nil {
				b.Fatalf("Execute: %v", err)
			}
		}
	}
}

// BenchmarkTaskStoreFlushSQLite is the commit a message or a terminal state
// pays: the token advances by one and the projection is written with it.
func BenchmarkTaskStoreFlushSQLite(b *testing.B) {
	store, _ := openStore(b)
	ctx := context.Background()
	if _, err := store.Create(ctx, a2aTask("task-1", "conv-1")); err != nil {
		b.Fatalf("Create: %v", err)
	}
	update := &taskstore.UpdateRequest{
		Task: &a2a.Task{
			ID: "task-1", ContextID: "conv-1",
			Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	version := taskstore.TaskVersion(1)
	for b.Loop() {
		update.PrevVersion = version
		next, err := store.Update(ctx, update)
		if err != nil {
			b.Fatalf("Update: %v", err)
		}
		version = next
	}
}
