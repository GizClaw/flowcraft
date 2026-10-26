package graph

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// A parallel branch runs a node handler on its own goroutine: a handler that
// panics must fail the wave, not the process.
func TestExecuteParallelBranchPanicFailsWave(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterType(reg, "entry", NodeType[struct{}]{
		Meta:    Meta{Desc: "entry"},
		Handler: func(ExecutionContext, *agent.Board, struct{}) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterType(reg, "panicking-branch", NodeType[struct{}]{
		Meta: Meta{Desc: "panics"},
		Handler: func(ExecutionContext, *agent.Board, struct{}) error {
			panic("node is broken")
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterType(reg, "quiet-branch", NodeType[struct{}]{
		Meta:    Meta{Desc: "succeeds"},
		Handler: func(ExecutionContext, *agent.Board, struct{}) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	g := mustBuild(t, &GraphDefinition{
		Name:  "parallel-panic",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "entry"},
			{ID: "b", Type: "panicking-branch"},
			{ID: "c", Type: "quiet-branch"},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: END}, {From: "c", To: END},
		},
	}, reg, WithParallel(ParallelConfig{Enabled: true}))

	_, err := g.Execute(context.Background(), testRun(), agent.NoopHost{}, agent.NewBoard())
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("Execute error = %v, want the recovered branch panic", err)
	}
}

func TestForkController_CancelRunningPropagatesCause(t *testing.T) {
	c := newForkController([]string{"a", "b"})
	ctx, cancel := context.WithCancelCause(context.Background())
	if _, cancelled := c.start("b", cancel); cancelled {
		t.Fatal("b unexpectedly cancelled at start")
	}

	if !c.view("a").CancelNode("b", "enough") {
		t.Fatal("CancelNode(b) = false, want true")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("branch context not cancelled")
	}
	if cause := context.Cause(ctx); cause == nil || cause.Error() != "enough" {
		t.Fatalf("context cause = %v, want the cancel reason", cause)
	}
	if reason, ok := c.cancelledReason("b"); !ok || reason != "enough" {
		t.Fatalf("cancelledReason = %q/%v", reason, ok)
	}
}

func TestForkController_CancelQueuedSkipsStart(t *testing.T) {
	c := newForkController([]string{"a", "b"})
	if !c.view("a").CancelNode("b", "not needed") {
		t.Fatal("CancelNode on queued branch = false")
	}
	// b starts later (it was stuck in the semaphore): the mark must
	// stop the invocation.
	if reason, cancelled := c.start("b", context.CancelCauseFunc(func(error) {})); !cancelled || reason != "not needed" {
		t.Fatalf("start = %q/%v, want the queued cancellation", reason, cancelled)
	}
}

func TestForkController_UnknownAndFinishedReportFalse(t *testing.T) {
	c := newForkController([]string{"a", "b"})
	if c.view("a").CancelNode("ghost", "x") {
		t.Fatal("unknown node cancelled")
	}
	_, cancel := context.WithCancelCause(context.Background())
	c.start("b", cancel)
	c.finish("b")
	if c.view("a").CancelNode("b", "too late") {
		t.Fatal("finished branch cancelled")
	}
}

func TestForkController_CancelledCallerLosesRights(t *testing.T) {
	c := newForkController([]string{"a", "b", "c"})
	_, cancelA := context.WithCancelCause(context.Background())
	_, cancelC := context.WithCancelCause(context.Background())
	c.start("a", cancelA)
	c.start("c", cancelC)

	if !c.view("b").CancelNode("a", "done") {
		t.Fatal("CancelNode(a) = false")
	}
	// a is cancelled: it may no longer cancel others.
	if c.view("a").CancelNode("c", "again") {
		t.Fatal("a cancelled branch cancelled another node")
	}
	if _, marked := c.cancelledReason("c"); marked {
		t.Fatal("c was marked by a disqualified caller")
	}
	// A healthy branch still can.
	if !c.view("b").CancelNode("c", "also done") {
		t.Fatal("healthy branch could not cancel c")
	}
}

func TestForkController_IdempotentDefaultReason(t *testing.T) {
	c := newForkController([]string{"a", "b"})
	_, cancel := context.WithCancelCause(context.Background())
	c.start("b", cancel)

	if !c.view("a").CancelNode("b", "") {
		t.Fatal("first cancel = false")
	}
	if !c.view("a").CancelNode("b", "second") {
		t.Fatal("repeat cancel = false, want idempotent true")
	}
	// First reason wins.
	if reason, _ := c.cancelledReason("b"); reason != "cancelled by parallel.cancelNode" {
		t.Fatalf("reason = %q, want the default from the first call", reason)
	}
}

func TestExecuteParallelInterruptMergesAssistantDeltasOnlyInBranchOrder(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterType(reg, "entry", NodeType[struct{}]{
		Meta:    Meta{Desc: "entry"},
		Handler: func(ExecutionContext, *agent.Board, struct{}) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	type branchConfig struct {
		Text string `json:"text"`
	}
	if err := RegisterType(reg, "interrupting-branch", NodeType[branchConfig]{
		Meta: Meta{Desc: "writes partial state then interrupts"},
		Handler: func(_ ExecutionContext, board *agent.Board, cfg branchConfig) error {
			board.SetVar("branch-"+cfg.Text, true)
			board.AppendChannelMessage(agent.MainChannel,
				message.NewTextMessage(message.RoleAssistant, cfg.Text))
			board.AppendChannelMessage("tools",
				message.NewTextMessage(message.RoleTool, "tool-"+cfg.Text))
			return agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput, Detail: cfg.Text})
		},
	}); err != nil {
		t.Fatal(err)
	}
	g := mustBuild(t, &GraphDefinition{
		Name:  "parallel-interrupt",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "entry"},
			{ID: "b", Type: "interrupting-branch", Config: []byte(`{"text":"b-partial"}`)},
			{ID: "c", Type: "interrupting-branch", Config: []byte(`{"text":"c-partial"}`)},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: END}, {From: "c", To: END},
		},
	}, reg, WithParallel(ParallelConfig{Enabled: true}))
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleUser, "input"))

	board, err := g.Execute(context.Background(), testRun(), agent.NoopHost{}, board)
	if !errdefs.IsInterrupted(err) {
		t.Fatalf("Execute error = %v, want interrupted", err)
	}
	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 3 ||
		msgs[1].Content.Text() != "b-partial" ||
		msgs[2].Content.Text() != "c-partial" {
		t.Fatalf("main channel = %+v, want input then deterministic b/c partials", msgs)
	}
	if got := board.Channel("tools"); len(got) != 0 {
		t.Fatalf("tool channel merged on interruption: %+v", got)
	}
	if _, ok := board.Var("branch-b-partial"); ok {
		t.Fatal("branch vars merged on interruption")
	}
	if _, ok := board.Var("branch-c-partial"); ok {
		t.Fatal("branch vars merged on interruption")
	}
}

func TestMergeInterruptedAssistantMessagesExcludesOrdinaryFailures(t *testing.T) {
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleUser, "input"))
	preFork := board.Snapshot()
	delta := func(text string) *agent.BoardDelta {
		branch := agent.NewBoard()
		branch.RestoreFrom(preFork)
		mark := branch.Mark()
		branch.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, text))
		return branch.Since(mark)
	}

	mergeInterruptedAssistantMessages(board, []BranchResult{
		{NodeID: "failed", Delta: delta("must-not-merge"), Err: errdefs.Internalf("failed")},
		{NodeID: "interrupted", Delta: delta("partial"), Err: agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput})},
	})

	messages := board.Channel(agent.MainChannel)
	if len(messages) != 2 || messages[1].Content.Text() != "partial" {
		t.Fatalf("main channel = %+v, want input and interrupted partial only", messages)
	}
}

// legacyFirstWriteWinsMerge is the pre-delta strategy, reimplemented from
// BranchResult.Snapshot exactly as the engine used to run it. The differential
// test below pins the delta path against it: the incremental merge must produce
// the same board as the absolute-state one, including the rule that a branch
// writing the value already on the board does not claim the key.
func legacyFirstWriteWinsMerge(_ context.Context, board *agent.Board, preFork *agent.BoardSnapshot, results []BranchResult) error {
	claimed := map[string]bool{}
	for _, res := range results {
		if res.Err != nil || res.Snapshot == nil {
			continue
		}
		for key, val := range res.Snapshot.Vars {
			if claimed[key] {
				continue
			}
			if prev, ok := preFork.Vars[key]; ok && reflect.DeepEqual(prev, val) {
				continue
			}
			board.SetVar(key, val)
			claimed[key] = true
		}
	}
	for _, res := range results {
		if res.Err != nil || res.Snapshot == nil {
			continue
		}
		for ch, msgs := range res.Snapshot.Channels {
			base := len(preFork.Channels[ch])
			if len(msgs) <= base {
				continue
			}
			for _, msg := range msgs[base:] {
				board.AppendChannelMessage(ch, msg)
			}
		}
	}
	return nil
}

// TestParallelDeltaMergeMatchesSnapshotMerge runs one wave twice — once through
// the built-in strategy, which consumes branch write sets, and once through a
// custom merge written against absolute branch state — and requires the same
// resulting board.
func TestParallelDeltaMergeMatchesSnapshotMerge(t *testing.T) {
	type writeConfig struct {
		Var   string `json:"var,omitempty"`
		Value string `json:"value,omitempty"`
		Msg   string `json:"msg,omitempty"`
	}
	build := func(t *testing.T, merge MergeFunc) *Graph {
		t.Helper()
		reg := NewRegistry()
		if err := RegisterType(reg, "entry", NodeType[struct{}]{
			Meta:    Meta{Desc: "entry"},
			Handler: func(ExecutionContext, *agent.Board, struct{}) error { return nil },
		}); err != nil {
			t.Fatal(err)
		}
		if err := RegisterType(reg, "write", NodeType[writeConfig]{
			Meta: Meta{Desc: "writes one var and one message"},
			Handler: func(_ ExecutionContext, board *agent.Board, cfg writeConfig) error {
				if cfg.Var != "" {
					board.SetVar(cfg.Var, cfg.Value)
				}
				if cfg.Msg != "" {
					board.AppendChannelMessage(agent.MainChannel,
						message.NewTextMessage(message.RoleAssistant, cfg.Msg))
				}
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		return mustBuild(t, &GraphDefinition{
			Name:  "parallel-differential",
			Entry: "a",
			Nodes: []NodeDefinition{
				{ID: "a", Type: "entry"},
				// writes the value the board already carries: not a claim
				{ID: "b", Type: "write", Config: []byte(`{"var":"shared","value":"seed","msg":"b"}`)},
				// writes a new value for the same key: the first real claim
				{ID: "c", Type: "write", Config: []byte(`{"var":"shared","value":"c-wins","msg":"c"}`)},
				{ID: "d", Type: "write", Config: []byte(`{"var":"own","value":"d","msg":"d"}`)},
			},
			Edges: []EdgeDefinition{
				{From: "a", To: "b"}, {From: "a", To: "c"}, {From: "a", To: "d"},
				{From: "b", To: END}, {From: "c", To: END}, {From: "d", To: END},
			},
		}, reg, WithParallel(ParallelConfig{
			Enabled:       true,
			MergeStrategy: FirstWriteWins,
			Merge:         merge,
		}))
	}
	seed := func() *agent.Board {
		board := agent.NewBoard()
		board.SetVar("shared", "seed")
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleUser, "input"))
		return board
	}

	incremental, err := build(t, nil).Execute(context.Background(), testRun(), agent.NoopHost{}, seed())
	if err != nil {
		t.Fatalf("delta merge: %v", err)
	}
	absolute, err := build(t, legacyFirstWriteWinsMerge).Execute(context.Background(), testRun(), agent.NoopHost{}, seed())
	if err != nil {
		t.Fatalf("snapshot merge: %v", err)
	}

	if got, want := incremental.Snapshot(), absolute.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("delta merge diverged from snapshot merge\n got: %#v\nwant: %#v", got, want)
	}
	// The rule the two paths have to agree on: a branch that writes the value
	// already present does not claim the key.
	if got, _ := incremental.Var("shared"); got != "c-wins" {
		t.Fatalf("shared = %v, want the first branch that changed it", got)
	}
}
