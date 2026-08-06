package graph

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
)

func TestExecutionContextEmitStreamDeltaStampsParallelBranchIdentity(t *testing.T) {
	host := &publishHost{}
	ctx := withParallelBranchIdentity(context.Background(), parallelBranchIdentity{
		forkID:   "fork-1",
		branchID: "branch-a",
	})
	ec := ExecutionContext{
		Context: ctx,
		Host:    host,
		NodeID:  "branch-a",
		GraphID: "g",
	}

	if err := ec.EmitStreamDelta(agent.StreamDeltaPayload{
		Type:    agent.StreamDeltaToken,
		Content: "partial",
	}); err != nil {
		t.Fatalf("EmitStreamDelta: %v", err)
	}

	deltas := streamDeltas(t, host)
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d, want 1", len(deltas))
	}
	if !deltas[0].Speculative ||
		deltas[0].ForkID != "fork-1" ||
		deltas[0].BranchID != "branch-a" {
		t.Fatalf("delta identity = %+v", deltas[0])
	}
}

func TestExecutionContextEmitStreamDeltaRejectsConflictingParallelIdentity(t *testing.T) {
	host := &publishHost{}
	ctx := withParallelBranchIdentity(context.Background(), parallelBranchIdentity{
		forkID:   "fork-1",
		branchID: "branch-a",
	})
	ec := ExecutionContext{
		Context: ctx,
		Host:    host,
		NodeID:  "branch-a",
		GraphID: "g",
	}

	for _, delta := range []agent.StreamDeltaPayload{
		{Type: agent.StreamDeltaToken, ForkID: "forged-fork"},
		{Type: agent.StreamDeltaToken, BranchID: "other-branch"},
	} {
		err := ec.EmitStreamDelta(delta)
		if !errdefs.IsConflict(err) && !errdefs.IsValidation(err) {
			t.Fatalf("error = %v, want conflict/validation", err)
		}
	}
	if got := len(streamDeltas(t, host)); got != 0 {
		t.Fatalf("conflicting deltas published = %d", got)
	}
}

func TestExecutionContextEmitStreamDeltaRejectsPluginParallelControls(t *testing.T) {
	host := &publishHost{}
	ctx := withParallelBranchIdentity(context.Background(), parallelBranchIdentity{
		forkID:   "fork-1",
		branchID: "branch-a",
	})
	ec := ExecutionContext{
		Context: ctx,
		Host:    host,
		NodeID:  "branch-a",
		GraphID: "g",
	}

	for _, delta := range []agent.StreamDeltaPayload{
		{
			Type:     agent.StreamDeltaParallelBranchAccept,
			ForkID:   "fork-1",
			BranchID: "branch-a",
		},
		{
			Type:     agent.StreamDeltaParallelBranchCancel,
			ForkID:   "fork-1",
			BranchID: "branch-a",
		},
		{
			Type:     agent.StreamDeltaParallelBranchAccept,
			ForkID:   "forged-fork",
			BranchID: "other-branch",
		},
		{
			Type:     agent.StreamDeltaParallelBranchCancel,
			ForkID:   "forged-fork",
			BranchID: "other-branch",
		},
	} {
		err := ec.EmitStreamDelta(delta)
		if !errdefs.IsValidation(err) {
			t.Fatalf("%s identity (%q, %q) error = %v, want validation",
				delta.Type, delta.ForkID, delta.BranchID, err)
		}
	}
	if got := len(streamDeltas(t, host)); got != 0 {
		t.Fatalf("plugin parallel controls published = %d", got)
	}
}

func TestExecuteParallelAcceptsOnlyAfterMergeInWaveOrder(t *testing.T) {
	var merged atomic.Bool
	host := &terminalOrderHost{merged: &merged}
	g := parallelTerminalGraph(t, func(ExecutionContext, string) error { return nil },
		WithParallel(ParallelConfig{
			Enabled: true,
			Merge: func(ctx context.Context, board *agent.Board, preFork *agent.BoardSnapshot, results []BranchResult) error {
				if err := firstWriteWinsMerge(ctx, board, preFork, results); err != nil {
					return err
				}
				merged.Store(true)
				return nil
			},
		}))

	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if host.acceptBeforeMerge.Load() {
		t.Fatal("parallel branch accepted before merge completed")
	}

	for _, delta := range host.dataDeltas(t) {
		if !delta.Speculative ||
			delta.ForkID == "" ||
			delta.BranchID != delta.Content {
			t.Fatalf("branch data identity = %+v", delta)
		}
	}
	terminals := host.terminals(t)
	if len(terminals) != 2 ||
		terminals[0].Type != agent.StreamDeltaParallelBranchAccept ||
		terminals[0].BranchID != "b" ||
		terminals[1].Type != agent.StreamDeltaParallelBranchAccept ||
		terminals[1].BranchID != "c" {
		t.Fatalf("terminals = %+v, want accept b then accept c", terminals)
	}
	if host.joinIndex() <= host.lastTerminalIndex() {
		t.Fatalf("join index %d must follow terminal index %d", host.joinIndex(), host.lastTerminalIndex())
	}
}

func TestExecuteParallelFailureCancelsEveryBranchExactlyOnce(t *testing.T) {
	g := parallelTerminalGraph(t, func(_ ExecutionContext, id string) error {
		if id == "b" {
			return errors.New("branch failed")
		}
		return nil
	})
	host := &terminalOrderHost{}

	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); err == nil {
		t.Fatal("Execute succeeded, want branch failure")
	}
	assertTerminalSet(t, host.terminals(t), map[string]agent.StreamDeltaType{
		"b": agent.StreamDeltaParallelBranchCancel,
		"c": agent.StreamDeltaParallelBranchCancel,
	})
	if host.joinIndex() != -1 {
		t.Fatal("failed wave published join")
	}
}

func TestExecuteParallelInterruptionCancelsEveryBranchExactlyOnce(t *testing.T) {
	g := parallelTerminalGraph(t, func(_ ExecutionContext, id string) error {
		if id == "b" {
			return agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput})
		}
		return nil
	})
	host := &terminalOrderHost{}

	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); !errdefs.IsInterrupted(err) {
		t.Fatalf("Execute error = %v, want interrupted", err)
	}
	assertTerminalSet(t, host.terminals(t), map[string]agent.StreamDeltaType{
		"b": agent.StreamDeltaParallelBranchCancel,
		"c": agent.StreamDeltaParallelBranchCancel,
	})
}

func TestExecuteParallelExplicitCancellationHasNoDuplicateTerminal(t *testing.T) {
	bStarted := make(chan struct{})
	g := parallelTerminalGraph(t, func(ec ExecutionContext, id string) error {
		switch id {
		case "b":
			close(bStarted)
			<-ec.Context.Done()
			return ec.Context.Err()
		case "c":
			<-bStarted
			controller, ok := ParallelControllerFromContext(ec.Context)
			if !ok || !controller.CancelNode("b", "not needed") {
				return errors.New("CancelNode(b) failed")
			}
		}
		return nil
	})
	host := &terminalOrderHost{}

	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertTerminalSet(t, host.terminals(t), map[string]agent.StreamDeltaType{
		"b": agent.StreamDeltaParallelBranchCancel,
		"c": agent.StreamDeltaParallelBranchAccept,
	})
}

func TestExecuteParallelCancelledBranchIgnoringContextIsCancelledAndNotMerged(t *testing.T) {
	bStarted := make(chan struct{})
	reg := NewRegistry()
	if err := RegisterType(reg, "entry", NodeType[struct{}]{
		Meta:    Meta{Desc: "entry"},
		Handler: func(ExecutionContext, *agent.Board, struct{}) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterType(reg, "ignores-cancel", NodeType[struct{}]{
		Meta: Meta{Desc: "ignores cancellation and returns success"},
		Handler: func(ec ExecutionContext, board *agent.Board, _ struct{}) error {
			close(bStarted)
			<-ec.Context.Done()
			board.SetVar("cancelled-output", "must-not-merge")
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterType(reg, "canceller", NodeType[struct{}]{
		Meta: Meta{Desc: "cancels the running sibling"},
		Handler: func(ec ExecutionContext, _ *agent.Board, _ struct{}) error {
			<-bStarted
			controller, ok := ParallelControllerFromContext(ec.Context)
			if !ok || !controller.CancelNode("b", "not needed") {
				return errors.New("CancelNode(b) failed")
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	g := mustBuild(t, &GraphDefinition{
		Name:  "cancel-ignoring-branch",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "entry"},
			{ID: "b", Type: "ignores-cancel"},
			{ID: "c", Type: "canceller"},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: END}, {From: "c", To: END},
		},
	}, reg, WithParallel(ParallelConfig{Enabled: true}))
	host := &terminalOrderHost{}

	board, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if value, ok := board.GetVar("cancelled-output"); ok {
		t.Fatalf("cancelled branch output merged: %v", value)
	}
	assertTerminalSet(t, host.terminals(t), map[string]agent.StreamDeltaType{
		"b": agent.StreamDeltaParallelBranchCancel,
		"c": agent.StreamDeltaParallelBranchAccept,
	})
}

func TestExecuteParallelMergeFailureCancelsEveryBranchExactlyOnce(t *testing.T) {
	g := parallelTerminalGraph(t, func(ExecutionContext, string) error { return nil },
		WithParallel(ParallelConfig{
			Enabled: true,
			Merge: func(context.Context, *agent.Board, *agent.BoardSnapshot, []BranchResult) error {
				return errors.New("merge failed")
			},
		}))
	host := &terminalOrderHost{}

	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); err == nil {
		t.Fatal("Execute succeeded, want merge failure")
	}
	assertTerminalSet(t, host.terminals(t), map[string]agent.StreamDeltaType{
		"b": agent.StreamDeltaParallelBranchCancel,
		"c": agent.StreamDeltaParallelBranchCancel,
	})
}

func TestExecuteParallelFailureTerminalPublishesAreBounded(t *testing.T) {
	g := parallelTerminalGraph(t, func(_ ExecutionContext, id string) error {
		if id == "b" {
			return errors.New("branch failed")
		}
		return nil
	}, WithParallel(ParallelConfig{Enabled: true}), WithRunEndPublishTimeout(30*time.Millisecond))
	host := &blockingTerminalHost{}

	start := time.Now()
	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); err == nil {
		t.Fatal("Execute succeeded, want branch failure")
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("Execute took %v with context-aware blocked terminal publisher", elapsed)
	}
	if got := host.calls.Load(); got < 2 {
		t.Fatalf("blocked terminal publishes = %d, want at least 2", got)
	}
}

func TestExecuteParallelControlPublishFailuresDoNotChangeResult(t *testing.T) {
	g := parallelTerminalGraph(t, func(ExecutionContext, string) error { return nil })
	host := &controlFailHost{}

	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); err != nil {
		t.Fatalf("control publish failure changed graph result: %v", err)
	}
}

func parallelTerminalGraph(
	t *testing.T,
	branch func(ExecutionContext, string) error,
	opts ...BuildOption,
) *Graph {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterType(reg, "entry", NodeType[struct{}]{
		Meta:    Meta{Desc: "entry"},
		Handler: func(ExecutionContext, *agent.Board, struct{}) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	type branchConfig struct {
		ID string `json:"id"`
	}
	if err := RegisterType(reg, "branch", NodeType[branchConfig]{
		Meta: Meta{Desc: "branch"},
		Handler: func(ec ExecutionContext, _ *agent.Board, cfg branchConfig) error {
			if err := ec.EmitStreamDelta(agent.StreamDeltaPayload{
				Type:    agent.StreamDeltaToken,
				Content: cfg.ID,
			}); err != nil {
				return err
			}
			return branch(ec, cfg.ID)
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(opts) == 0 {
		opts = []BuildOption{WithParallel(ParallelConfig{Enabled: true})}
	}
	return mustBuild(t, &GraphDefinition{
		Name:  "confirmed-stream",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "entry"},
			{ID: "b", Type: "branch", Config: []byte(`{"id":"b"}`)},
			{ID: "c", Type: "branch", Config: []byte(`{"id":"c"}`)},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: END}, {From: "c", To: END},
		},
	}, reg, opts...)
}

func streamDeltas(t *testing.T, host *publishHost) []agent.StreamDeltaPayload {
	t.Helper()
	host.mu.Lock()
	defer host.mu.Unlock()
	var deltas []agent.StreamDeltaPayload
	for _, env := range host.envs {
		if !agent.IsStreamDelta(env.Subject) {
			continue
		}
		delta, err := agent.DecodeStreamDelta(env)
		if err != nil {
			t.Fatalf("DecodeStreamDelta: %v", err)
		}
		deltas = append(deltas, delta)
	}
	return deltas
}

func assertTerminalSet(t *testing.T, got []agent.StreamDeltaPayload, want map[string]agent.StreamDeltaType) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("terminals = %+v, want exactly %d", got, len(want))
	}
	seen := make(map[string]int, len(got))
	for _, delta := range got {
		seen[delta.BranchID]++
		if delta.Type != want[delta.BranchID] {
			t.Fatalf("terminal for %q = %q, want %q", delta.BranchID, delta.Type, want[delta.BranchID])
		}
	}
	for branchID := range want {
		if seen[branchID] != 1 {
			t.Fatalf("branch %q terminal count = %d, want 1", branchID, seen[branchID])
		}
	}
}

type terminalOrderHost struct {
	agent.NoopHost
	mu                sync.Mutex
	envs              []event.Envelope
	merged            *atomic.Bool
	acceptBeforeMerge atomic.Bool
}

func (h *terminalOrderHost) Publish(_ context.Context, env event.Envelope) error {
	if agent.IsStreamDelta(env.Subject) {
		delta, _ := agent.DecodeStreamDelta(env)
		if delta.Type == agent.StreamDeltaParallelBranchAccept &&
			h.merged != nil && !h.merged.Load() {
			h.acceptBeforeMerge.Store(true)
		}
	}
	h.mu.Lock()
	h.envs = append(h.envs, env)
	h.mu.Unlock()
	return nil
}

func (h *terminalOrderHost) terminals(t *testing.T) []agent.StreamDeltaPayload {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []agent.StreamDeltaPayload
	for _, env := range h.envs {
		if !agent.IsStreamDelta(env.Subject) {
			continue
		}
		delta, err := agent.DecodeStreamDelta(env)
		if err != nil {
			t.Fatal(err)
		}
		switch delta.Type {
		case agent.StreamDeltaParallelBranchAccept, agent.StreamDeltaParallelBranchCancel:
			out = append(out, delta)
		}
	}
	return out
}

func (h *terminalOrderHost) dataDeltas(t *testing.T) []agent.StreamDeltaPayload {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []agent.StreamDeltaPayload
	for _, env := range h.envs {
		if !agent.IsStreamDelta(env.Subject) {
			continue
		}
		delta, err := agent.DecodeStreamDelta(env)
		if err != nil {
			t.Fatal(err)
		}
		switch delta.Type {
		case agent.StreamDeltaToken, agent.StreamDeltaToolCall, agent.StreamDeltaToolResult:
			out = append(out, delta)
		}
	}
	return out
}

func (h *terminalOrderHost) joinIndex() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, env := range h.envs {
		if env.Subject == agent.SubjectParallelJoin("run-1") {
			return i
		}
	}
	return -1
}

func (h *terminalOrderHost) lastTerminalIndex() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	last := -1
	for i, env := range h.envs {
		if !agent.IsStreamDelta(env.Subject) {
			continue
		}
		delta, _ := agent.DecodeStreamDelta(env)
		if delta.Type == agent.StreamDeltaParallelBranchAccept ||
			delta.Type == agent.StreamDeltaParallelBranchCancel {
			last = i
		}
	}
	return last
}

type controlFailHost struct {
	agent.NoopHost
}

func (h *controlFailHost) Publish(_ context.Context, env event.Envelope) error {
	if env.Subject == agent.SubjectParallelFork("run-1") ||
		env.Subject == agent.SubjectParallelJoin("run-1") {
		return errors.New("control unavailable")
	}
	if agent.IsStreamDelta(env.Subject) {
		delta, _ := agent.DecodeStreamDelta(env)
		if delta.Type == agent.StreamDeltaParallelBranchAccept ||
			delta.Type == agent.StreamDeltaParallelBranchCancel {
			return errors.New("control unavailable")
		}
	}
	return nil
}

type blockingTerminalHost struct {
	agent.NoopHost
	calls atomic.Int64
}

func (h *blockingTerminalHost) Publish(ctx context.Context, env event.Envelope) error {
	if !agent.IsStreamDelta(env.Subject) {
		return nil
	}
	delta, _ := agent.DecodeStreamDelta(env)
	if delta.Type != agent.StreamDeltaParallelBranchAccept &&
		delta.Type != agent.StreamDeltaParallelBranchCancel {
		return nil
	}
	h.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}
