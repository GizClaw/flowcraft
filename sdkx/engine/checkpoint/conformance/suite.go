package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
)

// Factory builds a fresh, empty store. The suite calls Factory once
// per subtest so subtests do not share state. Implementations MAY
// register cleanup against t (t.Cleanup) to drop the underlying
// connection / file when the subtest ends.
type Factory func(t *testing.T) engine.CheckpointStore

// RunSuite runs every contract test against the store produced by f.
//
// Subtests covering the optional CheckpointLister / CheckpointDeleter
// interfaces are auto-skipped when the store does not implement them.
// All other subtests are mandatory.
func RunSuite(t *testing.T, f Factory) {
	t.Helper()

	t.Run("SaveLoadRoundtrip", func(t *testing.T) { testSaveLoadRoundtrip(t, f) })
	t.Run("LoadMissing", func(t *testing.T) { testLoadMissing(t, f) })
	t.Run("SaveOverwriteLatest", func(t *testing.T) { testSaveOverwriteLatest(t, f) })
	t.Run("PreservesPayloadAndAttributes", func(t *testing.T) { testPreservesPayloadAndAttributes(t, f) })
	t.Run("ConcurrentSavesDifferentExecs", func(t *testing.T) { testConcurrentSavesDifferentExecs(t, f) })
	t.Run("List", func(t *testing.T) { testList(t, f) })
	t.Run("Delete", func(t *testing.T) { testDelete(t, f) })
}

// ---------- helpers ----------

func makeCheckpoint(execID string, step string, iteration int) engine.Checkpoint {
	board := engine.NewBoard()
	board.SetVar("greeting", "hello")
	return engine.Checkpoint{
		ExecID:    execID,
		Step:      step,
		Iteration: iteration,
		Board:     board.Snapshot(),
		Payload:   json.RawMessage(`{"engine":"graph","cursor":42}`),
		Attributes: map[string]string{
			"tenant":   "acme",
			"agent_id": "writer",
		},
		Timestamp: time.Now().UTC().Truncate(time.Millisecond),
	}
}

func equalCheckpoint(t *testing.T, want, got engine.Checkpoint) {
	t.Helper()
	if got.ExecID != want.ExecID {
		t.Errorf("ExecID: want %q got %q", want.ExecID, got.ExecID)
	}
	if got.Step != want.Step {
		t.Errorf("Step: want %q got %q", want.Step, got.Step)
	}
	if got.Iteration != want.Iteration {
		t.Errorf("Iteration: want %d got %d", want.Iteration, got.Iteration)
	}
	if got.Board == nil {
		t.Fatal("Board: nil")
	}
	if want.Board != nil && len(got.Board.Vars) != len(want.Board.Vars) {
		t.Errorf("Board.Vars len: want %d got %d", len(want.Board.Vars), len(got.Board.Vars))
	}
	if string(got.Payload) != string(want.Payload) {
		t.Errorf("Payload: want %s got %s", want.Payload, got.Payload)
	}
	if got.Attributes["tenant"] != want.Attributes["tenant"] {
		t.Errorf("Attributes[tenant]: want %q got %q",
			want.Attributes["tenant"], got.Attributes["tenant"])
	}
}

// ---------- subtests ----------

func testSaveLoadRoundtrip(t *testing.T, f Factory) {
	t.Helper()
	s := f(t)

	cp := makeCheckpoint("run-1", "node-a", 1)
	if err := s.Save(context.Background(), cp); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load: returned nil for an exec id we just saved")
	}
	equalCheckpoint(t, cp, *got)
}

func testLoadMissing(t *testing.T, f Factory) {
	t.Helper()
	s := f(t)

	got, err := s.Load(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("Load on missing exec id must return (nil, nil); got err=%v", err)
	}
	if got != nil {
		t.Errorf("Load on missing exec id must return (nil, nil); got %+v", got)
	}
}

func testSaveOverwriteLatest(t *testing.T, f Factory) {
	t.Helper()
	s := f(t)

	earlier := makeCheckpoint("run-2", "node-a", 1)
	later := makeCheckpoint("run-2", "node-b", 2)
	later.Timestamp = earlier.Timestamp.Add(time.Second)

	if err := s.Save(context.Background(), earlier); err != nil {
		t.Fatalf("Save earlier: %v", err)
	}
	if err := s.Save(context.Background(), later); err != nil {
		t.Fatalf("Save later: %v", err)
	}

	got, err := s.Load(context.Background(), "run-2")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load: nil after overwrite")
	}
	if got.Step != "node-b" {
		t.Errorf("Load did not return latest by Save order: Step=%q want %q", got.Step, "node-b")
	}
	if got.Iteration != 2 {
		t.Errorf("Load did not return latest iteration: %d want 2", got.Iteration)
	}
}

func testPreservesPayloadAndAttributes(t *testing.T, f Factory) {
	t.Helper()
	s := f(t)

	cp := makeCheckpoint("run-3", "node-a", 1)
	cp.Payload = json.RawMessage(`{"deeply":{"nested":[1,2,{"k":"v"}]}}`)
	cp.Attributes = map[string]string{
		"tenant":   "acme",
		"agent_id": "writer",
		"trace_id": "abc-123",
	}

	if err := s.Save(context.Background(), cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(context.Background(), "run-3")
	if err != nil || got == nil {
		t.Fatalf("Load: %v / %v", got, err)
	}
	if string(got.Payload) != string(cp.Payload) {
		t.Errorf("Payload not preserved verbatim:\n  want %s\n  got  %s",
			cp.Payload, got.Payload)
	}
	if len(got.Attributes) != len(cp.Attributes) {
		t.Errorf("Attributes count: want %d got %d", len(cp.Attributes), len(got.Attributes))
	}
	for k, v := range cp.Attributes {
		if got.Attributes[k] != v {
			t.Errorf("Attributes[%q]: want %q got %q", k, v, got.Attributes[k])
		}
	}
}

func testConcurrentSavesDifferentExecs(t *testing.T, f Factory) {
	t.Helper()
	s := f(t)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("run-conc-%d", i)
			if err := s.Save(context.Background(), makeCheckpoint(id, "node", i)); err != nil {
				t.Errorf("Save(%s): %v", id, err)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("run-conc-%d", i)
		got, err := s.Load(context.Background(), id)
		if err != nil {
			t.Errorf("Load(%s): %v", id, err)
			continue
		}
		if got == nil {
			t.Errorf("Load(%s): nil after concurrent Save", id)
		}
	}
}

func testList(t *testing.T, f Factory) {
	t.Helper()
	s := f(t)
	lister, ok := s.(engine.CheckpointLister)
	if !ok {
		t.Skip("store does not implement engine.CheckpointLister")
	}

	want := []string{"run-l-1", "run-l-2", "run-l-3"}
	for _, id := range want {
		if err := s.Save(context.Background(), makeCheckpoint(id, "node", 0)); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}

	got, err := lister.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(got)
	for _, id := range want {
		if !contains(got, id) {
			t.Errorf("List missing exec id %q (got %v)", id, got)
		}
	}
}

func testDelete(t *testing.T, f Factory) {
	t.Helper()
	s := f(t)
	deleter, ok := s.(engine.CheckpointDeleter)
	if !ok {
		t.Skip("store does not implement engine.CheckpointDeleter")
	}

	cp := makeCheckpoint("run-d-1", "node", 0)
	if err := s.Save(context.Background(), cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := deleter.Delete(context.Background(), "run-d-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := s.Load(context.Background(), "run-d-1")
	if err != nil {
		t.Fatalf("Load after Delete: %v", err)
	}
	if got != nil {
		t.Errorf("Load after Delete returned non-nil: %+v", got)
	}

	if err := deleter.Delete(context.Background(), "never-existed"); err != nil {
		t.Errorf("Delete on missing exec id must be a no-op; got err=%v", err)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
