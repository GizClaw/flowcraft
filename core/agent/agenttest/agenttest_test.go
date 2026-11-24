package agenttest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
)

// TestMemStorePassesTheStoreSuite keeps the suite honest: a real backend will run it, so
// the reference implementation must pass it first.
func TestMemStorePassesTheStoreSuite(t *testing.T) {
	agenttest.StoreSuite(t, func(*testing.T) agent.Store { return agenttest.NewMemStore() })
}

// TestScriptedPassesTheDriverSuite keeps the fixture honest: the suite a real driver
// will run must pass against the driver the other tests use, or the suite is wrong.
func TestScriptedPassesTheDriverSuite(t *testing.T) {
	agenttest.DriverSuite(t, func(*testing.T) agent.Program {
		return agenttest.NewScripted(
			agent.Text{Text: "hello"},
			agenttest.Completed(),
		)
	})
}

// TestScriptedReportsFailures pins that a scripted failure reaches the caller as an error
// rather than as a silent end of stream.
func TestScriptedReportsFailures(t *testing.T) {
	boom := errors.New("boom")
	driver := agenttest.NewScripted(agent.Text{Text: "x"}).FailAt(boom).OpenDriver()
	var got error
	for _, err := range driver.Run(context.Background(), agenttest.Request()) {
		if err != nil {
			got = err
			break
		}
	}
	if !errors.Is(got, boom) {
		t.Fatalf("err = %v, want the scripted one", got)
	}
}

// TestScriptedSteerQueueApplyUndo walks the whole steering window: queued, withdrawn
// before application, then queued again and applied at the boundary.
func TestScriptedSteerQueueApplyUndo(t *testing.T) {
	program := agenttest.NewScripted(agent.Text{Text: "before"}, agent.Text{Text: "after"}, agenttest.Completed())
	program.SteerAt = 1
	driver := program.OpenDriver()

	first, err := driver.Steer(agent.SteerInput{Message: agenttest.UserMessage("wrong"), Intent: agent.SteerCorrect})
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if err := driver.Unsteer(first); err != nil {
		t.Fatalf("Unsteer: %v", err)
	}
	if err := driver.Unsteer(first); !errors.Is(err, agent.ErrSteerUnknown) {
		t.Fatalf("second Unsteer = %v, want ErrSteerUnknown", err)
	}

	second, err := driver.Steer(agent.SteerInput{Message: agenttest.UserMessage("right"), Intent: agent.SteerRedirect})
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	_ = second

	applied := 0
	for event, err := range driver.Run(context.Background(), agenttest.Request()) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if _, ok := event.(agent.Interrupted); ok {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("applied %d interruptions, want 1 (the withdrawn one must not apply)", applied)
	}
	if got := driver.Applied(); len(got) != 1 || got[0].Intent != agent.SteerRedirect {
		t.Fatalf("applied = %+v", got)
	}
}

// TestMemStoreRoundTrip is the store contract's smallest form: the projection advances,
// events keep their order, and a blob reference is content-addressed.
func TestMemStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := agenttest.NewMemStore()

	first, last, err := store.Save(ctx, agent.Checkpoint{
		Task: agent.Task{ID: "t1", SessionID: "s1", State: agent.StateWorking},
	}, []agent.EventRecord{{Kind: "message", Payload: []byte(`"hi"`)}})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if first != 1 || last != 1 {
		t.Fatalf("seq = %d..%d, want 1..1", first, last)
	}

	loaded, err := store.Load(ctx, "t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != 1 || loaded.Task.State != agent.StateWorking {
		t.Fatalf("loaded = %+v", loaded)
	}
	if loaded.Task.History != nil {
		t.Fatal("the projection kept history; the event log is its only source")
	}

	latest, err := store.LatestForSession(ctx, "s1")
	if err != nil || latest.Task.ID != "t1" {
		t.Fatalf("LatestForSession = %+v, %v", latest, err)
	}

	events, err := store.Events(ctx, "t1")
	if err != nil || len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("events = %+v, %v", events, err)
	}

	ref, err := store.PutBlob(ctx, []byte("payload"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	again, err := store.PutBlob(ctx, []byte("payload"))
	if err != nil || again != ref {
		t.Fatalf("same bytes gave %q then %q", ref, again)
	}
	got, err := store.GetBlob(ctx, ref)
	if err != nil || string(got) != "payload" {
		t.Fatalf("GetBlob = %q, %v", got, err)
	}

	if err := store.Delete(ctx, "t1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load(ctx, "t1"); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Fatalf("Load after Delete = %v, want ErrNoCheckpoint", err)
	}
}

// TestMemStoreTruncateKeepsTheNewest protects the invariant that sequence numbers stay
// monotonic: a store must never drop the tail, or the next save reuses a number.
func TestMemStoreTruncateKeepsTheNewest(t *testing.T) {
	ctx := context.Background()
	store := agenttest.NewMemStore()
	if _, _, err := store.Save(ctx, agent.Checkpoint{Task: agent.Task{ID: "t1"}},
		[]agent.EventRecord{{Kind: "a"}, {Kind: "b"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.TruncateEvents(ctx, "t1", 9); err != nil {
		t.Fatalf("TruncateEvents: %v", err)
	}
	events, err := store.Events(ctx, "t1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want only the newest kept", len(events))
	}
	if events[0].Kind != "b" {
		t.Fatalf("kept %q, want the newest", events[0].Kind)
	}
}

func TestParseTraceLevel(t *testing.T) {
	for name, want := range map[string]agent.TraceLevel{
		"":     agent.TraceNone,
		"none": agent.TraceNone,
		"run":  agent.TraceRun,
		"call": agent.TraceCall,
		"node": agent.TraceNode,
	} {
		got, err := agent.ParseTraceLevel(name)
		if err != nil || got != want {
			t.Fatalf("ParseTraceLevel(%q) = %v, %v", name, got, err)
		}
	}
	if _, err := agent.ParseTraceLevel("everything"); err == nil {
		t.Fatal("an unknown level was accepted")
	}
	if agent.TraceCall.String() != "call" {
		t.Fatalf("String = %q", agent.TraceCall.String())
	}
}

// TestFailureBoxIsFirstWins mirrors the tool layer's rule: the innermost stage is closest
// to the cause, so an outer wrapper must not rewrite it.
func TestFailureBoxIsFirstWins(t *testing.T) {
	ctx := agent.WithFailureBox(context.Background())
	agent.RecordFailure(ctx, agent.FailurePolicyDenied)
	agent.RecordFailure(ctx, agent.FailureTimeout)
	if kind, ok := agent.FailureKindFrom(ctx); !ok || kind != agent.FailurePolicyDenied {
		t.Fatalf("kind = %q, ok = %v", kind, ok)
	}

	empty := agent.WithFailureBox(context.Background())
	if kind, ok := agent.FailureKindFrom(empty); !ok || kind != agent.FailureUnclassified {
		t.Fatalf("unclassified = %q, ok = %v", kind, ok)
	}
	if _, ok := agent.FailureKindFrom(context.Background()); ok {
		t.Fatal("a bare context reported a classification")
	}
}

// TestMemStoreCoversTheContractSurface walks the methods a real store backend must also
// provide, so the fixture proves the contract is implementable rather than only the paths
// the turn decorator happens to use.
func TestMemStoreCoversTheContractSurface(t *testing.T) {
	ctx := context.Background()
	store := agenttest.NewMemStore()

	if _, err := store.LatestForSession(ctx, "nobody"); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Fatalf("LatestForSession on an empty store = %v", err)
	}
	if _, err := store.Events(ctx, "missing"); err == nil {
		t.Fatal("Events returned a nil error for an unknown task")
	}
	if err := store.SetServerVersion(ctx, "missing", 3); err == nil {
		t.Fatal("SetServerVersion accepted an unknown task")
	}
	if err := store.TruncateEvents(ctx, "missing", 1); err == nil {
		t.Fatal("TruncateEvents accepted an unknown task")
	}
	if _, err := store.GetBlob(ctx, "sha256:none"); err == nil {
		t.Fatal("GetBlob returned a nil error for an unknown reference")
	}

	if _, _, err := store.Save(ctx, agent.Checkpoint{Task: agent.Task{ID: "t1", SessionID: "s1"}},
		[]agent.EventRecord{{Kind: "state"}, {Kind: "artifact"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.SetServerVersion(ctx, "t1", 9); err != nil {
		t.Fatalf("SetServerVersion: %v", err)
	}
	loaded, err := store.Load(ctx, "t1")
	if err != nil || loaded.ServerVersion != 9 {
		t.Fatalf("ServerVersion = %d, %v", loaded.ServerVersion, err)
	}

	// Kinds limits the deletion: an engine that compacts its state keeps the trace.
	if err := store.TruncateEvents(ctx, "t1", 9, "state"); err != nil {
		t.Fatalf("TruncateEvents: %v", err)
	}
	events, err := store.Events(ctx, "t1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, event := range events {
		if event.Kind == "state" {
			t.Fatal("a truncated kind survived")
		}
	}

	tasks, err := store.List(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("List = %+v, %v", tasks, err)
	}
}

// TestScriptedDriverReportsARunCount pins the fixture's own accounting, so a test that
// asserts "the driver ran once" is asserting something real.
func TestScriptedDriverReportsARunCount(t *testing.T) {
	program := agenttest.NewScripted(agenttest.Completed())
	driver := program.OpenDriver()
	for range 2 {
		for _, err := range driver.Run(context.Background(), agenttest.Request()) {
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
		}
	}
	if driver.Runs() != 2 {
		t.Fatalf("Runs = %d, want 2", driver.Runs())
	}
	if err := driver.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := driver.Steer(agent.SteerInput{}); !errors.Is(err, agent.ErrRunEnded) {
		t.Fatalf("Steer after Close = %v, want ErrRunEnded", err)
	}
}
