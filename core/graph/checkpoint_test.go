package graph

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
)

// chainGraph builds a linear graph of n echo nodes that each write one var and
// append one message, so every wave has something to persist.
func chainGraph(t *testing.T, nodes int) *Graph {
	t.Helper()
	reg := newTestRegistry(t)
	defs := make([]NodeDefinition, 0, nodes)
	edges := make([]EdgeDefinition, 0, nodes)
	for i := 0; i < nodes; i++ {
		id := fmt.Sprintf("n%d", i)
		defs = append(defs, NodeDefinition{ID: id, Type: "echo",
			Config: []byte(fmt.Sprintf(`{"set_var": "k%d", "set_val": %d}`, i, i))})
		if i+1 < nodes {
			edges = append(edges, EdgeDefinition{From: id, To: fmt.Sprintf("n%d", i+1)})
		} else {
			edges = append(edges, EdgeDefinition{From: id, To: END})
		}
	}
	return mustBuild(t, &GraphDefinition{Name: "chain", Entry: "n0", Nodes: defs, Edges: edges}, reg)
}

// TestWaveCheckpointsUseOneBaseThenDeltas proves a run persists its state once
// and then extends it: fifty waves used to re-encode the whole board each time.
func TestWaveCheckpointsUseOneBaseThenDeltas(t *testing.T) {
	const nodes = 5
	store := agenttest.NewJournalStore()
	host := agenttest.StoreHost{Store: store}
	g := chainGraph(t, nodes)

	board, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := store.BaseCount(); got != 1 {
		t.Fatalf("base records = %d, want 1 (the first wave)", got)
	}
	deltas := store.Deltas(testRun().RunID)
	if len(deltas) != nodes-1 {
		t.Fatalf("deltas = %d, want %d (one per wave after the base)", len(deltas), nodes-1)
	}
	for i, delta := range deltas {
		if got, want := delta.Iteration, i+2; got != want {
			t.Fatalf("delta %d iteration = %d, want %d", i, got, want)
		}
	}

	// A reader gets the same record shape it always did.
	cp, err := store.Load(context.Background(), testRun().RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cp == nil {
		t.Fatal("Load returned nil")
	}
	if iter := cp.Iteration; iter != nodes {
		t.Fatalf("checkpoint iteration = %d, want the last wave (%d)", iter, nodes)
	}
	if got, want := len(cp.Board.Channels[agent.MainChannel]), 0; got != want {
		t.Fatalf("main channel = %d messages, want %d", got, want)
	}
	for i := 0; i < nodes; i++ {
		key := fmt.Sprintf("k%d", i)
		replayed, ok := cp.Board.Vars[key]
		if !ok {
			t.Fatalf("resumed board is missing %q (the delta chain did not replay)", key)
		}
		if replayed != float64(i) {
			t.Fatalf("%s = %v, want %d", key, replayed, i)
		}
	}
	if final, _ := board.Var(fmt.Sprintf("k%d", nodes-1)); final != float64(nodes-1) {
		t.Fatalf("board var = %v, want the last node's write", final)
	}
}

// TestCheckpointCompactionWindow proves the window bounds the chain: a smaller
// window writes more bases and fewer appends.
func TestCheckpointCompactionWindow(t *testing.T) {
	const nodes = 5
	store := agenttest.NewJournalStore()
	host := agenttest.StoreHost{Store: store}
	reg := newTestRegistry(t)
	defs := []NodeDefinition{{ID: "a", Type: "echo"}, {ID: "b", Type: "echo"},
		{ID: "c", Type: "echo"}, {ID: "d", Type: "echo"}, {ID: "e", Type: "echo"}}
	edges := []EdgeDefinition{{From: "a", To: "b"}, {From: "b", To: "c"},
		{From: "c", To: "d"}, {From: "d", To: "e"}, {From: "e", To: END}}
	g := mustBuild(t, &GraphDefinition{Name: "window", Entry: "a", Nodes: defs, Edges: edges},
		reg, WithCheckpointCompaction(2))

	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Waves 1, 3 and 5 start a new base, so the chain is 2 deltas long twice
	// and empty at the end — a base discards what it replaces.
	if got, want := store.BaseCount(), 3; got != want {
		t.Fatalf("bases = %d, want %d with a window of 2 over %d waves", got, want, nodes)
	}
	if got := len(store.Deltas(testRun().RunID)); got != 0 {
		t.Fatalf("deltas = %d, want 0: the last wave wrote a base", got)
	}
	cp, err := store.Load(context.Background(), testRun().RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cp == nil {
		t.Fatal("Load returned nil")
	}
	// A windowed run still resumes: the last base is self-contained.
	if iter := cp.Iteration; iter != nodes {
		t.Fatalf("checkpoint iteration = %d, want %d", iter, nodes)
	}
}

// blobBoard is a board whose state is expensive to copy: the shape a long
// session with tool results or recovery blobs has.
func blobBoard(blobs, size int) *agent.Board {
	board := agent.NewBoard()
	for i := 0; i < blobs; i++ {
		board.SetVar(fmt.Sprintf("blob%d", i), make([]byte, size))
	}
	return board
}

func measureExecute(t *testing.T, g *Graph, host agent.Host, board *agent.Board) uint64 {
	t.Helper()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := g.Execute(context.Background(), testRun(), host, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestHostWithoutStoreCostsNoSnapshot is the guard for the capability check: a
// host that persists nothing must not copy the board at a wave boundary. Before
// the capability existed the snapshot was an argument to a no-op call, so every
// wave paid for a full board copy and threw it away.
func TestHostWithoutStoreCostsNoSnapshot(t *testing.T) {
	const (
		blobs     = 20
		blobSize  = 200 << 10 // 4 MB of board state
		boardSize = blobs * blobSize
	)
	g := chainGraph(t, 5)

	disabled := measureExecute(t, g, agenttest.StoreHost{}, blobBoard(blobs, blobSize))
	t.Logf("no store: %d bytes allocated for 5 waves over a %d MB board",
		disabled, boardSize>>20)
	if limit := uint64(boardSize) / 2; disabled > limit {
		t.Fatalf("a host with no store allocated %d bytes, want less than %d (half a board copy)",
			disabled, limit)
	}

	// A host that does not declare the capability at all keeps the previous
	// behaviour: one full record per wave.
	enabled := measureExecute(t, g, agent.NoopHost{}, blobBoard(blobs, blobSize))
	t.Logf("undeclared capability: %d bytes allocated (one record per wave)", enabled)
	if enabled < uint64(boardSize) {
		t.Fatalf("undeclared-capability host allocated %d bytes, want at least one board copy (%d)",
			enabled, boardSize)
	}
}

// TestJournalRunStaysUnderABoardCopyPerWave is the allocation ceiling that keeps
// the incremental path from regressing: a run whose host takes deltas may cost a
// few board copies in total, not one per wave.
func TestJournalRunStaysUnderABoardCopyPerWave(t *testing.T) {
	const (
		blobs     = 20
		blobSize  = 200 << 10
		boardSize = blobs * blobSize
		nodes     = 5
	)
	store := agenttest.NewJournalStore()
	host := agenttest.StoreHost{Store: store}
	g := chainGraph(t, nodes)

	alloc := measureExecute(t, g, host, blobBoard(blobs, blobSize))
	t.Logf("journal host: %d bytes allocated for %d waves over a %d MB board (%d bases)",
		alloc, nodes, boardSize>>20, store.BaseCount())

	// One base record costs a snapshot plus its encoding; the deltas are bytes.
	// The ceiling is generous — the point is that it does not scale with waves.
	if limit := uint64(boardSize) * 4; alloc > limit {
		t.Fatalf("journal run allocated %d bytes, want less than %d (4 board copies) for %d waves",
			alloc, limit, nodes)
	}
	if got := store.BaseCount(); got != 1 {
		t.Fatalf("bases = %d, want exactly one for a %d-wave run inside the window", got, nodes)
	}
}
