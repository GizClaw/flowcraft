package agent_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

func textMessage(text string) message.Message {
	return message.NewTextMessage(message.RoleAssistant, text)
}

// silentHost persists nothing and says so, which is what a deployment without a
// checkpoint store looks like.
type silentHost struct {
	agent.NoopHost
	writes int
}

func (h *silentHost) Checkpoint(context.Context, agent.Checkpoint) error {
	h.writes++
	return nil
}

func (h *silentHost) CheckpointCapabilities() agent.CheckpointCapabilities {
	return agent.CheckpointCapabilities{}
}

func TestCheckpointWriterSkipsDisabledHost(t *testing.T) {
	host := &silentHost{}
	writer := agent.NewCheckpointWriter(host, "run-1")
	if writer.Enabled() {
		t.Fatal("writer is enabled for a host that persists nothing")
	}
	board := agent.NewBoard()
	board.SetVar("x", 1)
	if err := writer.Stamp(context.Background(), agent.CheckpointMeta{Iteration: 1}, board); err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	if host.writes != 0 {
		t.Fatalf("host received %d writes, want none", host.writes)
	}
}

// plainHost persists checkpoints but declares nothing, the shape every host
// out there has today.
type plainHost struct {
	agent.NoopHost
	checkpoints []agent.Checkpoint
}

func (h *plainHost) Checkpoint(_ context.Context, cp agent.Checkpoint) error {
	h.checkpoints = append(h.checkpoints, cp)
	return nil
}

// TestCheckpointWriterUndeclaredHostKeepsFullRecords pins the conservative
// default: a host that does not declare the capability is treated as a
// full-record store, which is exactly what it was before this capability
// existed.
func TestCheckpointWriterUndeclaredHostKeepsFullRecords(t *testing.T) {
	host := &plainHost{}
	writer := agent.NewCheckpointWriter(host, "run-1")
	if !writer.Enabled() {
		t.Fatal("a host without the capability must still be given records")
	}
	board := agent.NewBoard()
	for i := 0; i < 3; i++ {
		board.SetVar("x", i)
		if err := writer.Stamp(context.Background(), agent.CheckpointMeta{Iteration: i + 1}, board); err != nil {
			t.Fatalf("Stamp %d: %v", i, err)
		}
	}
	if got := len(host.checkpoints); got != 3 {
		t.Fatalf("host received %d records, want one per boundary", got)
	}
	for i, cp := range host.checkpoints {
		if cp.Board == nil {
			t.Fatalf("record %d has no board", i)
		}
		if cp.Iteration != i+1 {
			t.Fatalf("record %d iteration = %d", i, cp.Iteration)
		}
	}
}

func TestCheckpointWriterBaseThenDeltas(t *testing.T) {
	store := agenttest.NewJournalStore()
	writer := agent.NewCheckpointWriter(agenttest.StoreHost{Store: store}, "run-1")
	board := agent.NewBoard()

	for i := 1; i <= 3; i++ {
		board.SetVar("step", i)
		board.AppendChannelMessage(agent.MainChannel, textMessage("m"+string(rune('0'+i))))
		if err := writer.Stamp(context.Background(), agent.CheckpointMeta{Iteration: i}, board); err != nil {
			t.Fatalf("Stamp %d: %v", i, err)
		}
	}
	if got := store.BaseCount(); got != 1 {
		t.Fatalf("bases = %d, want 1", got)
	}
	deltas := store.Deltas("run-1")
	if len(deltas) != 2 {
		t.Fatalf("deltas = %d, want 2", len(deltas))
	}
	// A delta carries the writes since the previous record, not the history.
	for i, delta := range deltas {
		if _, ok := delta.Board.Vars()["step"]; !ok {
			t.Fatalf("delta %d does not carry the var write", i)
		}
		if got := len(delta.Board.Appends()[agent.MainChannel]); got != 1 {
			t.Fatalf("delta %d appends %d messages, want 1", i, got)
		}
	}
	cp, err := store.Load(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(cp.Board.Channels[agent.MainChannel]); got != 3 {
		t.Fatalf("rebuilt channel = %d messages, want 3", got)
	}
}

// TestCheckpointWriterAsksForBaseWhenStoreSaysSo proves the writer answers
// ErrNeedsBase with a self-contained record instead of failing: a store may cap
// its own chain, and that must not cost the run its durability.
func TestCheckpointWriterAsksForBaseWhenStoreSaysSo(t *testing.T) {
	store := agenttest.NewJournalStore()
	writer := agent.NewCheckpointWriter(agenttest.StoreHost{Store: store}, "run-1")
	board := agent.NewBoard()

	board.SetVar("step", 1)
	if err := writer.Stamp(context.Background(), agent.CheckpointMeta{Iteration: 1}, board); err != nil {
		t.Fatalf("base Stamp: %v", err)
	}
	store.FailNext("append", agent.ErrNeedsBase)

	board.SetVar("step", 2)
	if err := writer.Stamp(context.Background(), agent.CheckpointMeta{Iteration: 2}, board); err != nil {
		t.Fatalf("Stamp after ErrNeedsBase: %v", err)
	}
	if got := store.BaseCount(); got != 2 {
		t.Fatalf("bases = %d, want 2 (the writer must fall back to a base)", got)
	}
	cp, err := store.Load(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The in-memory fake replays without a JSON round trip, so the value keeps
	// its Go type.
	if got, _ := cp.Board.Vars["step"]; fmt.Sprint(got) != "2" {
		t.Fatalf("step = %v, want the base's value", got)
	}
}

// TestCheckpointWriterKeepsPositionOnFailure proves a rejected write does not
// advance the writer: the next boundary retries from the same position instead
// of appending to a record that was never stored.
func TestCheckpointWriterKeepsPositionOnFailure(t *testing.T) {
	store := agenttest.NewJournalStore()
	writer := agent.NewCheckpointWriter(agenttest.StoreHost{Store: store}, "run-1")
	board := agent.NewBoard()

	board.SetVar("step", 1)
	if err := writer.Stamp(context.Background(), agent.CheckpointMeta{Iteration: 1}, board); err != nil {
		t.Fatalf("base Stamp: %v", err)
	}

	boom := errdefs.Internalf("store is down")
	store.FailNext("append", boom)
	board.SetVar("step", 2)
	if err := writer.Stamp(context.Background(), agent.CheckpointMeta{Iteration: 2}, board); !errors.Is(err, boom) {
		t.Fatalf("Stamp with a failing store = %v, want the store error", err)
	}

	board.SetVar("step", 3)
	if err := writer.Stamp(context.Background(), agent.CheckpointMeta{Iteration: 3}, board); err != nil {
		t.Fatalf("Stamp after recovery: %v", err)
	}
	cp, err := store.Load(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The recovered delta has to carry step 2 as well, because the failed
	// write never landed.
	if got, _ := cp.Board.Vars["step"]; fmt.Sprint(got) != "3" {
		t.Fatalf("step = %v, want 3", got)
	}
	if iter := cp.Iteration; iter != 3 {
		t.Fatalf("iteration = %d, want the last boundary", iter)
	}
}

// TestCheckpointWriterStampSnapshot proves the snapshot-input path (the runtime's
// session state) appends only what changed between the record it was given and
// the state it was asked to persist.
func TestCheckpointWriterStampSnapshot(t *testing.T) {
	store := agenttest.NewJournalStore()
	writer := agent.NewCheckpointWriterForStore(store, "session-1")
	ctx := context.Background()

	base := agent.NewBoard()
	base.SetVar("kept", "same")
	base.AppendChannelMessage(agent.MainChannel, textMessage("one"))

	if err := writer.StampSnapshot(ctx, agent.CheckpointMeta{Payload: []byte(`{"n":1}`)}, nil, base.Snapshot()); err != nil {
		t.Fatalf("StampSnapshot base: %v", err)
	}
	if got := store.BaseCount(); got != 1 {
		t.Fatalf("bases = %d, want 1", got)
	}

	next := agent.NewBoard()
	next.RestoreFrom(base.Snapshot())
	next.SetVar("added", "new")
	next.AppendChannelMessage(agent.MainChannel, textMessage("two"))

	if err := writer.StampSnapshot(ctx, agent.CheckpointMeta{Payload: []byte(`{"n":2}`)},
		base.Snapshot(), next.Snapshot()); err != nil {
		t.Fatalf("StampSnapshot delta: %v", err)
	}

	deltas := store.Deltas("session-1")
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d, want 1", len(deltas))
	}
	if _, ok := deltas[0].Board.Vars()["kept"]; ok {
		t.Fatal("delta carries an unchanged var")
	}
	if _, ok := deltas[0].Board.Vars()["added"]; !ok {
		t.Fatal("delta is missing the new var")
	}
	if got := len(deltas[0].Board.Appends()[agent.MainChannel]); got != 1 {
		t.Fatalf("delta appends %d messages, want only the new one", got)
	}

	cp, err := store.Load(ctx, "session-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(cp.Board.Channels[agent.MainChannel]); got != 2 {
		t.Fatalf("rebuilt channel = %d messages, want 2", got)
	}
	if string(cp.Payload) != `{"n":2}` {
		t.Fatalf("payload = %s, want the delta's payload", cp.Payload)
	}
}
