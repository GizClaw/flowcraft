package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
)

func TestLocalExecutor_ParallelForkJoin(t *testing.T) {
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"branch_a": newTestNode("branch_a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("a_result", "done_a")
				return nil
			}),
			"branch_b": newTestNode("branch_b", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("b_result", "done_b")
				return nil
			}),
			"join": newTestNode("join", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("joined", true)
				return nil
			}),
		},
		[]graph.Edge{
			{From: "start", To: "branch_a"},
			{From: "start", To: "branch_b"},
			{From: "branch_a", To: "join"},
			{From: "branch_b", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board,
		WithParallel(ParallelConfig{
			Enabled:       true,
			MaxBranches:   10,
			MergeStrategy: MergeLastWins,
		}),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if v := result.GetVarString("a_result"); v != "done_a" {
		t.Fatalf("expected done_a, got %q", v)
	}
	if v := result.GetVarString("b_result"); v != "done_b" {
		t.Fatalf("expected done_b, got %q", v)
	}
	if v, _ := result.GetVar("joined"); v != true {
		t.Fatal("expected joined=true")
	}
}

func TestLocalExecutor_ParallelNamespaceMerge(t *testing.T) {
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"a": newTestNode("a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("output", "from_a")
				return nil
			}),
			"b": newTestNode("b", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("output", "from_b")
				return nil
			}),
			"join": graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "a"},
			{From: "start", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board,
		WithParallel(ParallelConfig{
			MaxBranches:   10,
			MergeStrategy: MergeNamespace,
		}),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	a := result.GetVarString("__branch_0.output")
	b := result.GetVarString("__branch_1.output")
	if a == "" || b == "" {
		t.Fatalf("expected namespaced vars, got a=%q b=%q", a, b)
	}
}

func TestLocalExecutor_ParallelForkJoin_IndependentResolvers(t *testing.T) {
	var aConfig, bConfig atomic.Value

	nodeA := &configurableTestNode{
		id:     "branch_a",
		config: map[string]any{"label": "${board.query}_a"},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			aConfig.Store(cfg["label"])
			b.SetVar("a_done", true)
			return nil
		},
	}
	nodeB := &configurableTestNode{
		id:     "branch_b",
		config: map[string]any{"label": "${board.query}_b"},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			bConfig.Store(cfg["label"])
			b.SetVar("b_done", true)
			return nil
		},
	}

	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start":    graph.NewPassthroughNode("start", "passthrough"),
			"branch_a": nodeA,
			"branch_b": nodeB,
			"join":     graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "branch_a"},
			{From: "start", To: "branch_b"},
			{From: "branch_a", To: "join"},
			{From: "branch_b", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	board.SetVar("query", "test")

	resolver := variable.NewResolver()
	resolver.AddScope("board", board.Vars())

	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board,
		WithResolver(resolver),
		WithParallel(ParallelConfig{
			Enabled:       true,
			MaxBranches:   10,
			MergeStrategy: MergeLastWins,
		}),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if v, _ := result.GetVar("a_done"); v != true {
		t.Fatal("expected a_done=true")
	}
	if v, _ := result.GetVar("b_done"); v != true {
		t.Fatal("expected b_done=true")
	}
}

func TestLocalExecutor_ParallelForkJoin_BranchError(t *testing.T) {
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"ok": newTestNode("ok", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("ok_done", true)
				return nil
			}),
			"fail": newTestNode("fail", func(_ graph.ExecutionContext, b *graph.Board) error {
				return fmt.Errorf("branch failed")
			}),
			"join": graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "ok"},
			{From: "start", To: "fail"},
			{From: "ok", To: "join"},
			{From: "fail", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board,
		WithParallel(ParallelConfig{
			Enabled:     true,
			MaxBranches: 10,
		}),
	)
	if err == nil {
		t.Fatal("expected error from failing branch")
	}
}

func TestLocalExecutor_ErrorOnConflictMerge(t *testing.T) {
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"a": newTestNode("a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("shared", "from_a")
				return nil
			}),
			"b": newTestNode("b", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("shared", "from_b")
				return nil
			}),
			"join": graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "a"},
			{From: "start", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board,
		WithParallel(ParallelConfig{
			MaxBranches:   10,
			MergeStrategy: MergeErrorOnConflict,
		}),
	)
	if err == nil {
		t.Fatal("expected conflict error from parallel merge")
	}
}

func TestLocalExecutor_TypedResolution_ParallelBranches(t *testing.T) {
	var aCapturedTemp, bCapturedTemp atomic.Value

	nodeA := &configurableTestNode{
		id:     "branch_a",
		config: map[string]any{"temperature": "${board.temperature}"},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			aCapturedTemp.Store(cfg["temperature"])
			b.SetVar("a_done", true)
			return nil
		},
	}
	nodeB := &configurableTestNode{
		id:     "branch_b",
		config: map[string]any{"temperature": "${board.temperature}"},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			bCapturedTemp.Store(cfg["temperature"])
			b.SetVar("b_done", true)
			return nil
		},
	}

	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start":    graph.NewPassthroughNode("start", "passthrough"),
			"branch_a": nodeA,
			"branch_b": nodeB,
			"join":     graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "branch_a"},
			{From: "start", To: "branch_b"},
			{From: "branch_a", To: "join"},
			{From: "branch_b", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	board.SetVar("temperature", 0.7)

	resolver := variable.NewResolver()
	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board,
		WithResolver(resolver),
		WithParallel(ParallelConfig{
			Enabled:       true,
			MaxBranches:   10,
			MergeStrategy: MergeLastWins,
		}),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if v, _ := result.GetVar("a_done"); v != true {
		t.Fatal("expected a_done=true")
	}
	if v, _ := result.GetVar("b_done"); v != true {
		t.Fatal("expected b_done=true")
	}

	aTemp, _ := aCapturedTemp.Load().(float64)
	bTemp, _ := bCapturedTemp.Load().(float64)
	if aTemp != 0.7 {
		t.Fatalf("branch_a temperature: expected 0.7, got %v", aCapturedTemp.Load())
	}
	if bTemp != 0.7 {
		t.Fatalf("branch_b temperature: expected 0.7, got %v", bCapturedTemp.Load())
	}

	if nodeA.config["temperature"] != "${board.temperature}" {
		t.Fatalf("branch_a config should be restored, got %v", nodeA.config["temperature"])
	}
	if nodeB.config["temperature"] != "${board.temperature}" {
		t.Fatalf("branch_b config should be restored, got %v", nodeB.config["temperature"])
	}
}

func TestLocalExecutor_ParallelCancelNode_DropsCanceledBranch(t *testing.T) {
	draftStarted := make(chan struct{})
	lateCancelResult := make(chan bool, 1)
	host := &recordingParallelHost{}

	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"decide": newTestNode("decide", func(ctx graph.ExecutionContext, b *graph.Board) error {
				select {
				case <-draftStarted:
				case <-time.After(time.Second):
					return fmt.Errorf("draft branch did not start")
				}
				controller, ok := graph.ParallelControllerFromContext(ctx.Context)
				if !ok {
					return fmt.Errorf("parallel controller missing")
				}
				if !controller.CancelNode("draft", "intent rejected draft") {
					return fmt.Errorf("cancelNode returned false")
				}
				b.SetVar("decision_result", "keep_decision")
				return nil
			}),
			"draft": newTestNode("draft", func(ctx graph.ExecutionContext, b *graph.Board) error {
				controller, ok := graph.ParallelControllerFromContext(ctx.Context)
				if !ok {
					return fmt.Errorf("parallel controller missing")
				}
				close(draftStarted)
				ctx.Publisher.Emit("token", map[string]any{"content": "draft partial"})
				<-ctx.Context.Done()
				ctx.Publisher.Emit("token", map[string]any{"content": "late after cancel"})
				lateCancelResult <- controller.CancelNode("decide", "canceled branch should not cancel sibling")
				b.SetVar("draft_result", "should_not_merge")
				return ctx.Context.Err()
			}),
			"join": newTestNode("join", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("joined", true)
				return nil
			}),
		},
		[]graph.Edge{
			{From: "start", To: "decide"},
			{From: "start", To: "draft"},
			{From: "decide", To: "join"},
			{From: "draft", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	result, err := NewLocalExecutor().Execute(context.Background(), g, graph.NewBoard(),
		WithRunID("run-cancel"),
		WithHost(host),
		WithParallel(ParallelConfig{Enabled: true, MaxBranches: 10}),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if v := result.GetVarString("decision_result"); v != "keep_decision" {
		t.Fatalf("decision_result = %q", v)
	}
	if v := result.GetVarString("draft_result"); v != "" {
		t.Fatalf("canceled draft branch merged result %q", v)
	}
	if v, _ := result.GetVar("joined"); v != true {
		t.Fatal("expected joined=true")
	}
	select {
	case got := <-lateCancelResult:
		if got {
			t.Fatal("canceled branch was allowed to cancel sibling")
		}
	default:
		t.Fatal("canceled branch did not attempt late sibling cancel")
	}

	payload, ok := host.firstPayload(subjParallelBranchCancel("run-cancel"))
	if !ok {
		t.Fatal("expected parallel branch cancel event")
	}
	if payload["reason"] != "intent rejected draft" {
		t.Fatalf("cancel reason = %v", payload["reason"])
	}
	if payload["node_id"] != "draft" || payload["branch_id"] != "draft" {
		t.Fatalf("cancel payload = %+v", payload)
	}
	if payload["fork_id"] != "run-cancel:start" {
		t.Fatalf("fork_id = %v", payload["fork_id"])
	}
	cancelDelta, ok := host.firstStreamPayload(engine.StreamDeltaParallelBranchCancel)
	if !ok {
		t.Fatal("expected stream control delta for branch cancel")
	}
	if cancelDelta.ForkID != "run-cancel:start" ||
		cancelDelta.BranchID != "draft" ||
		cancelDelta.Reason != "intent rejected draft" ||
		!cancelDelta.Speculative {
		t.Fatalf("cancel stream delta = %+v", cancelDelta)
	}

	var draftToken map[string]any
	for _, env := range host.events() {
		if env.NodeID() != "draft" {
			continue
		}
		var payload map[string]any
		if err := env.Decode(&payload); err != nil {
			t.Fatalf("decode draft stream payload: %v", err)
		}
		if payload["type"] == "token" {
			draftToken = payload
			break
		}
	}
	if draftToken == nil {
		t.Fatal("expected speculative token from canceled draft branch")
	}
	if draftToken["speculative"] != true ||
		draftToken["fork_id"] != "run-cancel:start" ||
		draftToken["branch_id"] != "draft" {
		t.Fatalf("draft token parallel metadata = %+v", draftToken)
	}
	for _, env := range host.events() {
		if env.NodeID() != "draft" {
			continue
		}
		var payload map[string]any
		if err := env.Decode(&payload); err != nil {
			t.Fatalf("decode draft stream payload: %v", err)
		}
		if payload["content"] == "late after cancel" {
			t.Fatalf("stream delta emitted after branch cancel: %+v", payload)
		}
	}
	for _, payload := range host.payloads(subjParallelBranchAccept("run-cancel")) {
		if payload["branch_id"] == "draft" {
			t.Fatalf("canceled draft branch was accepted: %+v", payload)
		}
	}
}

func TestLocalExecutor_ParallelNamespaceMerge_PreservesOriginalIndexAfterCancel(t *testing.T) {
	dropDone := make(chan struct{})
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"drop": newTestNode("drop", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("dropped", true)
				close(dropDone)
				return nil
			}),
			"keep": newTestNode("keep", func(ctx graph.ExecutionContext, b *graph.Board) error {
				select {
				case <-dropDone:
				case <-time.After(time.Second):
					return fmt.Errorf("drop branch did not complete")
				}
				controller, ok := graph.ParallelControllerFromContext(ctx.Context)
				if !ok {
					return fmt.Errorf("parallel controller missing")
				}
				if !controller.CancelNode("drop", "drop first branch output") {
					return fmt.Errorf("cancelNode returned false")
				}
				b.SetVar("kept", true)
				return nil
			}),
			"join": graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "drop"},
			{From: "start", To: "keep"},
			{From: "drop", To: "join"},
			{From: "keep", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	result, err := NewLocalExecutor().Execute(context.Background(), g, graph.NewBoard(),
		WithRunID("run-namespace-cancel"),
		WithParallel(ParallelConfig{Enabled: true, MaxBranches: 10, MergeStrategy: MergeNamespace}),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if v, _ := result.GetVar("__branch_0.dropped"); v != nil {
		t.Fatalf("canceled branch 0 should not merge, got %v", v)
	}
	if v, _ := result.GetVar("__branch_0.kept"); v != nil {
		t.Fatalf("kept branch was renumbered to branch 0: %v", v)
	}
	if v, _ := result.GetVar("__branch_1.kept"); v != true {
		t.Fatalf("kept branch should preserve original namespace __branch_1, got %v", v)
	}
}

func TestLocalExecutor_ParallelCancelNode_CancelsCompletedOutput(t *testing.T) {
	draftDone := make(chan struct{})
	host := &recordingParallelHost{}

	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"decide": newTestNode("decide", func(ctx graph.ExecutionContext, b *graph.Board) error {
				select {
				case <-draftDone:
				case <-time.After(time.Second):
					return fmt.Errorf("draft branch did not complete")
				}
				controller, ok := graph.ParallelControllerFromContext(ctx.Context)
				if !ok {
					return fmt.Errorf("parallel controller missing")
				}
				if !controller.CancelNode("draft", "completed output rejected") {
					return fmt.Errorf("cancelNode returned false")
				}
				b.SetVar("decision_result", "done")
				return nil
			}),
			"draft": newTestNode("draft", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("draft_result", "completed_but_rejected")
				close(draftDone)
				return nil
			}),
			"join": graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "decide"},
			{From: "start", To: "draft"},
			{From: "decide", To: "join"},
			{From: "draft", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	result, err := NewLocalExecutor().Execute(context.Background(), g, graph.NewBoard(),
		WithRunID("run-completed-cancel"),
		WithHost(host),
		WithParallel(ParallelConfig{Enabled: true, MaxBranches: 10}),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if got := result.GetVarString("draft_result"); got != "" {
		t.Fatalf("completed-but-canceled branch merged draft_result %q", got)
	}
	if got := result.GetVarString("decision_result"); got != "done" {
		t.Fatalf("decision_result = %q", got)
	}

	cancelPayload, ok := host.firstPayload(subjParallelBranchCancel("run-completed-cancel"))
	if !ok {
		t.Fatal("expected cancel event for completed branch output")
	}
	if cancelPayload["reason"] != "completed output rejected" {
		t.Fatalf("cancel reason = %v", cancelPayload["reason"])
	}
	for _, payload := range host.payloads(subjParallelBranchAccept("run-completed-cancel")) {
		if payload["branch_id"] == "draft" {
			t.Fatalf("completed-but-canceled draft branch was accepted: %+v", payload)
		}
	}
}

func TestLocalExecutor_ParallelBranchError_CancelsSpeculativeBranches(t *testing.T) {
	slowStarted := make(chan struct{})
	host := &recordingParallelHost{}

	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"slow": newTestNode("slow", func(ctx graph.ExecutionContext, _ *graph.Board) error {
				close(slowStarted)
				ctx.Publisher.Emit("token", map[string]any{"content": "partial"})
				<-ctx.Context.Done()
				return ctx.Context.Err()
			}),
			"fail": newTestNode("fail", func(_ graph.ExecutionContext, _ *graph.Board) error {
				select {
				case <-slowStarted:
				case <-time.After(time.Second):
					return fmt.Errorf("slow branch did not start")
				}
				return fmt.Errorf("branch failed")
			}),
			"join": graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "slow"},
			{From: "start", To: "fail"},
			{From: "slow", To: "join"},
			{From: "fail", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	_, err := NewLocalExecutor().Execute(context.Background(), g, graph.NewBoard(),
		WithRunID("run-error-cancel"),
		WithHost(host),
		WithParallel(ParallelConfig{Enabled: true, MaxBranches: 10}),
	)
	if err == nil {
		t.Fatal("expected branch failure")
	}

	cancelPayloads := host.payloads(subjParallelBranchCancel("run-error-cancel"))
	if len(cancelPayloads) == 0 {
		t.Fatal("expected cancel terminal events on branch failure")
	}
	cancelByBranch := make(map[string]map[string]any)
	for _, payload := range cancelPayloads {
		if branchID, _ := payload["branch_id"].(string); branchID != "" {
			cancelByBranch[branchID] = payload
		}
	}
	if _, ok := cancelByBranch["slow"]; !ok {
		t.Fatalf("missing cancel event for slow branch: %+v", cancelPayloads)
	}
	if _, ok := cancelByBranch["fail"]; !ok {
		t.Fatalf("missing cancel event for failed branch: %+v", cancelPayloads)
	}
	for branchID, payload := range cancelByBranch {
		if reason, _ := payload["reason"].(string); reason == "" {
			t.Fatalf("cancel reason missing for branch %s: %+v", branchID, payload)
		}
	}
	if got := host.payloads(subjParallelBranchAccept("run-error-cancel")); len(got) != 0 {
		t.Fatalf("erroring fork should not accept branches: %+v", got)
	}
}

func TestLocalExecutor_ParallelStreamEvents_AreSpeculativeThenAccepted(t *testing.T) {
	host := &recordingParallelHost{}
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"emitter": newTestNode("emitter", func(ctx graph.ExecutionContext, b *graph.Board) error {
				ctx.Publisher.Emit("token", map[string]any{"content": "hello"})
				b.SetVar("emitted", true)
				return nil
			}),
			"other": newTestNode("other", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("other_done", true)
				return nil
			}),
			"join": graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "emitter"},
			{From: "start", To: "other"},
			{From: "emitter", To: "join"},
			{From: "other", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	if _, err := NewLocalExecutor().Execute(context.Background(), g, graph.NewBoard(),
		WithRunID("run-stream"),
		WithHost(host),
		WithParallel(ParallelConfig{Enabled: true, MaxBranches: 10}),
	); err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	var tokenPayload map[string]any
	for _, env := range host.events() {
		if env.NodeID() != "emitter" {
			continue
		}
		var payload map[string]any
		if err := env.Decode(&payload); err != nil {
			t.Fatalf("decode stream payload: %v", err)
		}
		if payload["type"] == "token" {
			tokenPayload = payload
			break
		}
	}
	if tokenPayload == nil {
		t.Fatal("expected token stream event from emitter")
	}
	if tokenPayload["speculative"] != true {
		t.Fatalf("speculative = %v", tokenPayload["speculative"])
	}
	if tokenPayload["fork_id"] != "run-stream:start" || tokenPayload["branch_id"] != "emitter" {
		t.Fatalf("parallel metadata = %+v", tokenPayload)
	}

	acceptPayload, ok := host.firstPayload(subjParallelBranchAccept("run-stream"))
	if !ok {
		t.Fatal("expected branch accept event")
	}
	if acceptPayload["fork_id"] != "run-stream:start" {
		t.Fatalf("accept fork_id = %v", acceptPayload["fork_id"])
	}
	acceptDeltas := host.streamPayloads(engine.StreamDeltaParallelBranchAccept)
	acceptedBranches := make(map[string]bool, len(acceptDeltas))
	for _, delta := range acceptDeltas {
		acceptedBranches[delta.BranchID] = true
		if delta.ForkID != "run-stream:start" || !delta.Speculative {
			t.Fatalf("accept stream delta = %+v", delta)
		}
	}
	if !acceptedBranches["emitter"] {
		t.Fatalf("expected emitter branch accept stream delta, got %+v", acceptDeltas)
	}
}

func TestLocalExecutor_ParallelMergeConflict_CancelsUnacceptedBranches(t *testing.T) {
	host := &recordingParallelHost{}
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"a": newTestNode("a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("shared", "from_a")
				return nil
			}),
			"b": newTestNode("b", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("shared", "from_b")
				return nil
			}),
			"join": graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "a"},
			{From: "start", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	_, err := NewLocalExecutor().Execute(context.Background(), g, graph.NewBoard(),
		WithRunID("run-merge-conflict"),
		WithHost(host),
		WithParallel(ParallelConfig{Enabled: true, MaxBranches: 10, MergeStrategy: MergeErrorOnConflict}),
	)
	if err == nil {
		t.Fatal("expected merge conflict")
	}
	cancelPayloads := host.payloads(subjParallelBranchCancel("run-merge-conflict"))
	if len(cancelPayloads) != 2 {
		t.Fatalf("cancel payload count = %d, want 2: %+v", len(cancelPayloads), cancelPayloads)
	}
	for _, payload := range cancelPayloads {
		if reason, _ := payload["reason"].(string); !strings.Contains(reason, "merge failed") {
			t.Fatalf("cancel reason should mention merge failure: %+v", payload)
		}
	}
	if got := host.payloads(subjParallelBranchAccept("run-merge-conflict")); len(got) != 0 {
		t.Fatalf("merge conflict should not accept branches: %+v", got)
	}
	cancelDeltas := host.streamPayloads(engine.StreamDeltaParallelBranchCancel)
	cancelDeltaBranches := make(map[string]bool, len(cancelDeltas))
	for _, delta := range cancelDeltas {
		cancelDeltaBranches[delta.BranchID] = true
	}
	if len(cancelDeltaBranches) != 2 || !cancelDeltaBranches["a"] || !cancelDeltaBranches["b"] {
		t.Fatalf("cancel stream delta branches = %+v, payloads=%+v", cancelDeltaBranches, cancelDeltas)
	}
}

type recordingParallelHost struct {
	engine.NoopHost

	mu   sync.Mutex
	envs []event.Envelope
}

func (h *recordingParallelHost) Publish(_ context.Context, env event.Envelope) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.envs = append(h.envs, env)
	return nil
}

func (h *recordingParallelHost) events() []event.Envelope {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]event.Envelope, len(h.envs))
	copy(out, h.envs)
	return out
}

func (h *recordingParallelHost) firstPayload(subject event.Subject) (map[string]any, bool) {
	payloads := h.payloads(subject)
	if len(payloads) == 0 {
		return nil, false
	}
	return payloads[0], true
}

func (h *recordingParallelHost) firstStreamPayload(deltaType engine.StreamDeltaType) (engine.StreamDeltaPayload, bool) {
	payloads := h.streamPayloads(deltaType)
	if len(payloads) == 0 {
		return engine.StreamDeltaPayload{}, false
	}
	return payloads[0], true
}

func (h *recordingParallelHost) streamPayloads(deltaType engine.StreamDeltaType) []engine.StreamDeltaPayload {
	var out []engine.StreamDeltaPayload
	for _, env := range h.events() {
		if !engine.IsStreamDelta(env.Subject) {
			continue
		}
		payload, err := engine.DecodeStreamDelta(env)
		if err != nil || payload.Type != deltaType {
			continue
		}
		out = append(out, payload)
	}
	return out
}

func (h *recordingParallelHost) payloads(subject event.Subject) []map[string]any {
	var out []map[string]any
	for _, env := range h.events() {
		if env.Subject != subject {
			continue
		}
		var payload map[string]any
		if err := env.Decode(&payload); err != nil {
			continue
		}
		out = append(out, payload)
	}
	return out
}
