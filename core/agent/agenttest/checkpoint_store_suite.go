package agenttest

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// CheckpointStoreSuite exercises the [agent.CheckpointStore] contract:
// absent-load semantics, Save/Load round trips, last-write-wins
// ordering, caller ownership on both Save and Load, and concurrent
// use. newStore must return a fresh, ready-to-use store for every
// call.
//
// The optional CheckpointLister / CheckpointDeleter interfaces are
// exercised when the store implements them; otherwise those subtests
// skip.
func CheckpointStoreSuite(t *testing.T, newStore func() agent.CheckpointStore) {
	t.Helper()

	t.Run("LoadAbsent", func(t *testing.T) {
		t.Helper()
		store := newStore()
		got, err := store.Load(context.Background(), "missing-run")
		if err != nil {
			t.Fatalf("Load absent: %v", err)
		}
		if got != nil {
			t.Fatalf("Load absent = %+v, want nil", got)
		}
	})

	t.Run("SaveLoadRoundTrip", func(t *testing.T) {
		t.Helper()
		store := newStore()
		cp := checkpointStoreSample("run-1")
		if err := store.Save(context.Background(), cp); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := store.Load(context.Background(), "run-1")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got == nil {
			t.Fatal("Load after Save returned nil")
		}
		if !checkpointEqual(*got, cp) {
			t.Fatalf("Load = %+v, want %+v", *got, cp)
		}
	})

	t.Run("LastWriteWins", func(t *testing.T) {
		t.Helper()
		store := newStore()
		first := checkpointStoreSample("run-1")
		first.Steps = []string{"wave-1"}
		second := checkpointStoreSample("run-1")
		second.Steps = []string{"wave-2"}

		if err := store.Save(context.Background(), first); err != nil {
			t.Fatalf("Save first: %v", err)
		}
		if err := store.Save(context.Background(), second); err != nil {
			t.Fatalf("Save second: %v", err)
		}
		got, err := store.Load(context.Background(), "run-1")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got == nil || !checkpointEqual(*got, second) {
			t.Fatalf("Load = %+v, want second %+v", got, second)
		}
	})

	t.Run("SaveTakesOwnership", func(t *testing.T) {
		t.Helper()
		store := newStore()
		cp := checkpointStoreSample("run-1")
		if err := store.Save(context.Background(), cp); err != nil {
			t.Fatalf("Save: %v", err)
		}

		cp.Steps[0] = "mutated"
		cp.Board.Vars["x"] = float64(99)
		cp.Attributes["tenant"] = "mutated"
		cp.Payload[0] = 'x'

		got, err := store.Load(context.Background(), "run-1")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got == nil || got.Steps[0] != "wave-1" || got.Board.Vars["x"] != float64(1) ||
			string(got.Payload) != `{"task_id":"t1"}` || got.Attributes["tenant"] != "tenant-a" {
			t.Fatalf("stored checkpoint mutated by caller: %+v", got)
		}
	})

	t.Run("LoadReturnsIndependentCopy", func(t *testing.T) {
		t.Helper()
		store := newStore()
		cp := checkpointStoreSample("run-1")
		if err := store.Save(context.Background(), cp); err != nil {
			t.Fatalf("Save: %v", err)
		}

		first, err := store.Load(context.Background(), "run-1")
		if err != nil {
			t.Fatalf("Load first: %v", err)
		}
		first.Board.Vars["x"] = float64(99)
		first.Steps[0] = "mutated"

		second, err := store.Load(context.Background(), "run-1")
		if err != nil {
			t.Fatalf("Load second: %v", err)
		}
		if second == nil || second.Board.Vars["x"] != float64(1) || second.Steps[0] != "wave-1" {
			t.Fatalf("Load result aliases store state: %+v", second)
		}
	})

	t.Run("ConcurrentAccess", func(t *testing.T) {
		t.Helper()
		store := newStore()
		ctx := context.Background()
		const workers = 16
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				execID := fmt.Sprintf("run-%d", i)
				if err := store.Save(ctx, checkpointStoreSample(execID)); err != nil {
					t.Errorf("Save %s: %v", execID, err)
					return
				}
				got, err := store.Load(ctx, execID)
				if err != nil {
					t.Errorf("Load %s: %v", execID, err)
					return
				}
				if got == nil || got.ExecID != execID {
					t.Errorf("Load %s = %+v, want matching record", execID, got)
				}
			}()
		}
		wg.Wait()
	})

	t.Run("OptionalList", func(t *testing.T) {
		t.Helper()
		store := newStore()
		lister, ok := store.(agent.CheckpointLister)
		if !ok {
			t.Skip("store does not implement CheckpointLister")
		}
		if err := store.Save(context.Background(), checkpointStoreSample("run-a")); err != nil {
			t.Fatalf("Save run-a: %v", err)
		}
		if err := store.Save(context.Background(), checkpointStoreSample("run-b")); err != nil {
			t.Fatalf("Save run-b: %v", err)
		}
		ids, err := lister.List(context.Background())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if !containsString(ids, "run-a") || !containsString(ids, "run-b") {
			t.Fatalf("List = %v, want run-a and run-b", ids)
		}
	})

	t.Run("OptionalDelete", func(t *testing.T) {
		t.Helper()
		store := newStore()
		deleter, ok := store.(agent.CheckpointDeleter)
		if !ok {
			t.Skip("store does not implement CheckpointDeleter")
		}
		if err := store.Save(context.Background(), checkpointStoreSample("run-1")); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := deleter.Delete(context.Background(), "run-1"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, err := store.Load(context.Background(), "run-1")
		if err != nil {
			t.Fatalf("Load after Delete: %v", err)
		}
		if got != nil {
			t.Fatalf("Load after Delete = %+v, want nil", got)
		}
	})
}

func checkpointStoreSample(execID string) agent.Checkpoint {
	// Fixed whole-second timestamps keep the identity checks portable:
	// stores that truncate sub-second precision must still pass.
	ts := time.Unix(1_700_000_000, 0)
	return agent.Checkpoint{
		ExecID:            execID,
		Steps:             []string{"wave-1"},
		Iteration:         3,
		Board:             checkpointStoreBoard(),
		Payload:           []byte(`{"task_id":"t1"}`),
		Attributes:        map[string]string{"tenant": "tenant-a"},
		Timestamp:         ts,
		OriginalStartedAt: ts,
		SpecVersion:       "v1",
	}
}

func checkpointStoreBoard() *agent.BoardSnapshot {
	return &agent.BoardSnapshot{
		Vars: map[string]any{
			"x":     float64(1),
			"items": []any{"a", "b"},
		},
		Channels: map[string][]message.Message{
			agent.MainChannel: {message.NewTextMessage(message.RoleAssistant, "hi")},
		},
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func checkpointEqual(a, b agent.Checkpoint) bool {
	if !a.OriginalStartedAt.Equal(b.OriginalStartedAt) {
		return false
	}
	// Timestamp is advisory: [agent.Checkpoint] explicitly allows
	// hosts to overwrite it when they persist, and sub-second
	// precision is not portable across store backends. OriginalStartedAt
	// is compared separately above because resume plumbing
	// (agent.LoadAndResume) threads it through the store.
	a.Timestamp = time.Time{}
	b.Timestamp = time.Time{}
	a.OriginalStartedAt = time.Time{}
	b.OriginalStartedAt = time.Time{}
	return reflect.DeepEqual(a, b)
}
