package agent_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/identity/inject"
)

// A turn runs once per model round, and its cost is dominated by what the driver does.
// What these measure is the decorator's own overhead — the part every driver pays and
// none of them can avoid: context derivation, identity, the fold over the stream, and the
// terminal save.

func benchScript(b *testing.B, events int) *agenttest.ScriptedDriver {
	b.Helper()
	script := make([]agent.Event, 0, events+1)
	for range events {
		script = append(script, agent.Text{Text: "chunk"})
	}
	script = append(script, agenttest.Completed())
	return agenttest.NewScripted(script...).OpenDriver()
}

// benchSession is sessionCtx for benchmarks, which have no *testing.T. A benchmark that
// ran without an attached session would measure the warning path instead of the turn.
func benchSession(b *testing.B) context.Context {
	b.Helper()
	ctx, err := inject.Session(context.Background(), identity.Session{
		Tenant:    "acme",
		Agent:     "helper",
		SessionID: "s1",
		Bind:      "bind-1",
	})
	if err != nil {
		b.Fatalf("inject.Session: %v", err)
	}
	return ctx
}

// BenchmarkRunTurnNoStore is the floor: a turn that keeps nothing.
func BenchmarkRunTurnNoStore(b *testing.B) {
	ctx := benchSession(b)
	req := request(nil)
	driver := benchScript(b, 8)
	b.ReportAllocs()
	for b.Loop() {
		agent.RunTurn(ctx, req, driver)(func(agent.Event, error) bool { return true })
	}
}

// BenchmarkRunTurnWithStore adds the terminal save and the event log, which is what a
// durable deployment pays on every turn.
func BenchmarkRunTurnWithStore(b *testing.B) {
	ctx := benchSession(b)
	store := agenttest.NewMemStore()
	req := request(store)
	driver := benchScript(b, 8)
	b.ReportAllocs()
	for b.Loop() {
		agent.RunTurn(ctx, req, driver)(func(agent.Event, error) bool { return true })
	}
}

// BenchmarkRunTurnLongStream is the same turn with a realistic number of deltas: the fold
// is O(events), and this is the number that says whether it is.
func BenchmarkRunTurnLongStream(b *testing.B) {
	ctx := benchSession(b)
	req := request(nil)
	driver := benchScript(b, 512)
	b.ReportAllocs()
	for b.Loop() {
		agent.RunTurn(ctx, req, driver)(func(agent.Event, error) bool { return true })
	}
}

// BenchmarkRunTurnConsumerStops measures the early-exit path: a caller that saw enough
// still pays the cancellation and the terminal save.
func BenchmarkRunTurnConsumerStops(b *testing.B) {
	ctx := benchSession(b)
	store := agenttest.NewMemStore()
	req := request(store)
	driver := benchScript(b, 64)
	b.ReportAllocs()
	for b.Loop() {
		agent.RunTurn(ctx, req, driver)(func(agent.Event, error) bool { return false })
	}
}

// BenchmarkMemStoreSave is the store half: a projection write plus a handful of facts.
func BenchmarkMemStoreSave(b *testing.B) {
	ctx := context.Background()
	store := agenttest.NewMemStore()
	records := []agent.EventRecord{
		{Kind: "state", Payload: []byte(`{"state":"working"}`)},
		{Kind: "artifact", Payload: []byte(`{"id":"a1"}`)},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := store.Save(ctx, agent.Checkpoint{
			Fingerprint: "fp-1",
			Task:        agent.Task{ID: "t1", SessionID: "s1", State: agent.StateWorking},
		}, records); err != nil {
			b.Fatalf("Save: %v", err)
		}
	}
}

// BenchmarkMemStoreLoad is the resume read: the projection only, never the log.
func BenchmarkMemStoreLoad(b *testing.B) {
	ctx := context.Background()
	store := agenttest.NewMemStore()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Task: agent.Task{ID: "t1", SessionID: "s1"},
	}, nil); err != nil {
		b.Fatalf("Save: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := store.Load(ctx, "t1"); err != nil {
			b.Fatalf("Load: %v", err)
		}
	}
}
