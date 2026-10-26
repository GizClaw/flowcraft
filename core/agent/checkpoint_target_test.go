package agent_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
)

// plainStore is a store that only implements [agent.CheckpointStore]: it takes
// self-contained records and nothing else.
type plainStore struct{ inner *agenttest.JournalStore }

func (s plainStore) Save(ctx context.Context, cp agent.Checkpoint) error {
	return s.inner.Save(ctx, cp)
}

func (s plainStore) Load(ctx context.Context, execID string) (*agent.Checkpoint, error) {
	return s.inner.Load(ctx, execID)
}

// withholdsCheckpoints suppresses the checkpoint capability while forwarding
// everything else — the shape an ephemeral turn needs.
type withholdsCheckpoints struct {
	agent.Host
}

func (h withholdsCheckpoints) CheckpointCapabilities() agent.CheckpointCapabilities {
	return agent.CheckpointCapabilities{}
}

// TestCheckpointCapabilitiesFromHost is the table that guards the seam the whole
// design rests on: what an engine believes about persistence decides whether it
// snapshots the board at all, and whether a write bypasses a decorator.
func TestCheckpointCapabilitiesFromHost(t *testing.T) {
	journal := agenttest.NewJournalStore()
	plain := plainStore{inner: agenttest.NewJournalStore()}

	tests := []struct {
		name        string
		host        agent.Host
		wantEnabled bool
		wantJournal bool
	}{
		{
			name:        "noop host does not declare the capability",
			host:        agent.NoopHost{},
			wantEnabled: true, // conservative default: full records
		},
		{
			name:        "host that declares nothing",
			host:        &plainHost{},
			wantEnabled: true,
		},
		{
			name:        "store without a journal",
			host:        agenttest.StoreHost{Store: plain},
			wantEnabled: true,
		},
		{
			name:        "store with a journal",
			host:        agenttest.StoreHost{Store: journal},
			wantEnabled: true,
			wantJournal: true,
		},
		{
			name:        "store host with no store",
			host:        agenttest.StoreHost{},
			wantEnabled: false,
		},
		{
			name: "delegating HostFuncs",
			host: agent.HostFuncs{Inner: agenttest.StoreHost{Store: journal}},
			// HostFuncs forwards, and routes the journal through itself.
			wantEnabled: true,
			wantJournal: true,
		},
		{
			name: "HostFuncs that replaces the writer",
			host: agent.HostFuncs{
				Inner:        agenttest.StoreHost{Store: journal},
				CheckpointFn: func(ctx context.Context, cp agent.Checkpoint) error { return nil },
			},
			wantEnabled: true,
			wantJournal: false, // a replacement may store full records only
		},
		{
			name:        "tracing decorator",
			host:        agent.ComposeHost(agenttest.StoreHost{Store: journal}, agent.TracingMiddleware()),
			wantEnabled: true,
			wantJournal: true,
		},
		{
			name:        "decorator that withholds the capability",
			host:        withholdsCheckpoints{Host: agenttest.StoreHost{Store: journal}},
			wantEnabled: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Helper()
			caps := agent.CheckpointCapabilitiesFromHost(tc.host)
			if caps.Enabled != tc.wantEnabled {
				t.Fatalf("Enabled = %v, want %v", caps.Enabled, tc.wantEnabled)
			}
			if got := caps.Journal != nil; got != tc.wantJournal {
				t.Fatalf("Journal present = %v, want %v", got, tc.wantJournal)
			}
		})
	}
}

// TestCheckpointWriterNilHostIsANoOp pins the documented promise: a nil host
// yields a writer whose Stamp does nothing, so callers need no branch.
func TestCheckpointWriterNilHostIsANoOp(t *testing.T) {
	var host agent.Host
	writer := agent.NewCheckpointWriter(host, "run-1")
	if writer.Enabled() {
		t.Fatal("writer is enabled without a host")
	}
	board := agent.NewBoard()
	board.SetVar("x", 1)
	if err := writer.Stamp(context.Background(), agent.CheckpointMeta{Iteration: 1}, board); err != nil {
		t.Fatalf("Stamp: %v", err)
	}
}

// TestCheckpointWriterNilBaseStartsABase proves a save whose caller cannot say
// what the store holds writes a self-contained record: appending there would
// extend a chain the caller cannot see.
func TestCheckpointWriterNilBaseStartsABase(t *testing.T) {
	store := agenttest.NewJournalStore()
	writer := agent.NewCheckpointWriterForStore(store, "session-1")
	ctx := context.Background()

	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, textMessage("one"))
	// First save: base, and the chain is open.
	if err := writer.StampSnapshot(ctx, agent.CheckpointMeta{Payload: []byte(`{"n":1}`)}, nil, board.Snapshot()); err != nil {
		t.Fatalf("StampSnapshot base: %v", err)
	}
	// Second save with no base (a failed load looks exactly like this): it has
	// to replace the record, not append the whole channel onto it again.
	board.AppendChannelMessage(agent.MainChannel, textMessage("two"))
	if err := writer.StampSnapshot(ctx, agent.CheckpointMeta{Payload: []byte(`{"n":2}`)}, nil, board.Snapshot()); err != nil {
		t.Fatalf("StampSnapshot with nil base: %v", err)
	}
	if got := store.BaseCount(); got != 2 {
		t.Fatalf("bases = %d, want 2 (a save with no base must rewrite the record)", got)
	}
	cp, err := store.Load(ctx, "session-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(cp.Board.Channels[agent.MainChannel]); got != 2 {
		t.Fatalf("rebuilt channel = %d messages, want 2 (history must not be duplicated)", got)
	}
}
