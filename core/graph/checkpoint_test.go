package graph

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/errdefs"
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
	// The resume anchor travels on the delta too: without it a resume from a
	// journal-backed record would restart at the entry node.
	if got, want := cp.Steps, []string{fmt.Sprintf("n%d", nodes-1)}; !slices.Equal(got, want) {
		t.Fatalf("Steps = %v, want the last wave %v", got, want)
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
	// A window of 2 allows two appends after a base: waves 1 (base), 2 and 3
	// (appends), 4 (base), 5 (append) — so two bases and one surviving delta,
	// because a base discards the chain it replaces.
	if got, want := store.BaseCount(), 2; got != want {
		t.Fatalf("bases = %d, want %d with a window of 2 over %d waves", got, want, nodes)
	}
	if got, want := len(store.Deltas(testRun().RunID)), 1; got != want {
		t.Fatalf("deltas = %d, want %d", got, want)
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
	// behaviour: one full record per wave, so a five-wave run costs about five
	// board copies. The floor is three, which a "snapshot once and reuse"
	// regression no longer satisfies.
	enabled := measureExecute(t, g, agent.NoopHost{}, blobBoard(blobs, blobSize))
	t.Logf("undeclared capability: %d bytes allocated (one record per wave)", enabled)
	if limit := uint64(boardSize) * 3; enabled < limit {
		t.Fatalf("undeclared-capability host allocated %d bytes, want at least %d (three board copies)",
			enabled, limit)
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

	// Compare against a single-wave run rather than an absolute constant: a
	// journal run pays for one base at the first boundary and nothing
	// board-sized afterwards, so growth over the extra four waves must stay
	// under one board copy. A relative bound survives a different Go version,
	// a loaded runner, or a change in how many copies a base costs.
	// A separate store, so the baseline does not add a base to the run above.
	oneWave := measureExecute(t, chainGraph(t, 1),
		agenttest.StoreHost{Store: agenttest.NewJournalStore()}, blobBoard(blobs, blobSize))
	growth := int64(alloc) - int64(oneWave)
	t.Logf("journal host: %d bytes for one wave, growth over %d more waves: %d bytes",
		oneWave, nodes-1, growth)
	if growth > 0 && uint64(growth) > uint64(boardSize) {
		t.Fatalf("journal run grew by %d bytes over %d waves, want less than one board copy (%d)",
			growth, nodes-1, boardSize)
	}
	// A regression to full records per wave would also fail this: ~5 bases.
	if limit := uint64(boardSize) * 6; alloc > limit {
		t.Fatalf("journal run allocated %d bytes, want less than %d", alloc, limit)
	}
	if got := store.BaseCount(); got != 1 {
		t.Fatalf("bases = %d, want exactly one for a %d-wave run inside the window", got, nodes)
	}
}

// TestResumeFromAJournalChain proves the whole loop the design is for: a run
// persists through a journalling store, Load rebuilds the record, and a resume
// continues from the checkpointed frontier instead of restarting at the entry.
func TestResumeFromAJournalChain(t *testing.T) {
	const nodes = 4
	reg := newTestRegistry(t)
	defs := make([]NodeDefinition, 0, nodes)
	edges := make([]EdgeDefinition, 0, nodes)
	for i := 0; i < nodes; i++ {
		id := fmt.Sprintf("n%d", i)
		// A message per node makes a re-run of the entry observable.
		defs = append(defs, NodeDefinition{ID: id, Type: "echo",
			Config: []byte(fmt.Sprintf(`{"message": "step-%d"}`, i))})
		if i+1 < nodes {
			edges = append(edges, EdgeDefinition{From: id, To: fmt.Sprintf("n%d", i+1)})
		} else {
			edges = append(edges, EdgeDefinition{From: id, To: END})
		}
	}
	def := &GraphDefinition{Name: "resume-chain", Entry: "n0", Nodes: defs, Edges: edges}
	store := agenttest.NewJournalStore()
	host := agenttest.StoreHost{Store: store}

	first := mustBuild(t, def, reg)
	if _, err := first.Execute(context.Background(), testRun(), host, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cp, err := store.Load(context.Background(), testRun().RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cp == nil {
		t.Fatal("no checkpoint after the run")
	}
	if got := len(cp.Board.Channels[agent.MainChannel]); got != nodes {
		t.Fatalf("checkpointed channel = %d messages, want %d", got, nodes)
	}

	resumed := testRun()
	resumed.ResumeFrom = cp
	board, err := mustBuild(t, def, reg).Execute(context.Background(), resumed, host, agent.RestoreBoard(nil))
	if err != nil {
		t.Fatalf("resumed Execute: %v", err)
	}
	// n3 routes to END, so the resumed frontier is empty: a resume that lost
	// Steps would replay n0 and append its message a second time.
	if got := len(board.Channel(agent.MainChannel)); got != nodes {
		t.Fatalf("resumed channel = %d messages, want %d (the run must not restart at the entry)", got, nodes)
	}
}

// TestCheckpointCompactionDefaultsAndValidation pins the two values a deployment
// can express without naming a number: zero means the engine default, and a
// negative value is a build error.
func TestCheckpointCompactionDefaultsAndValidation(t *testing.T) {
	reg := newTestRegistry(t)
	def := &GraphDefinition{
		Name:  "compaction",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}},
		Edges: []EdgeDefinition{{From: "a", To: END}},
	}
	if g := mustBuild(t, def, reg); g.checkpointCompaction != 0 {
		t.Fatalf("default compaction = %d, want 0 (the writer's default applies)", g.checkpointCompaction)
	}
	if g := mustBuild(t, def, reg, WithCheckpointCompaction(4)); g.checkpointCompaction != 4 {
		t.Fatalf("compaction = %d, want 4", g.checkpointCompaction)
	}
	if _, err := Build(def, reg, WithCheckpointCompaction(-1)); !errdefs.IsValidation(err) {
		t.Fatalf("Build with a negative window = %v, want validation", err)
	}
}
