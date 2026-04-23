package executor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
)

func TestLocalExecutor_SimplePassthrough(t *testing.T) {
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: graph.END},
		},
	)

	board := graph.NewBoard()
	board.SetVar("query", "hello")

	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if v := result.GetVarString("query"); v != "hello" {
		t.Fatalf("expected 'hello', got %q", v)
	}
}

func TestLocalExecutor_TwoNodePipeline(t *testing.T) {
	g := buildGraph("test", "a",
		map[string]graph.Node{
			"a": newTestNode("a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("step", "a_done")
				return nil
			}),
			"b": newTestNode("b", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("step", "b_done")
				return nil
			}),
		},
		[]graph.Edge{
			{From: "a", To: "b"},
			{From: "b", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if v := result.GetVarString("step"); v != "b_done" {
		t.Fatalf("expected 'b_done', got %q", v)
	}
}

func TestLocalExecutor_ConditionalRouting(t *testing.T) {
	condTrue, _ := graph.CompileCondition("route == true")
	condFalse, _ := graph.CompileCondition("route == false")

	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"true_br": newTestNode("true_br", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("result", "took_true")
				return nil
			}),
			"false_br": newTestNode("false_br", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("result", "took_false")
				return nil
			}),
		},
		[]graph.Edge{
			{From: "start", To: "true_br", Condition: condTrue},
			{From: "start", To: "false_br", Condition: condFalse},
			{From: "true_br", To: graph.END},
			{From: "false_br", To: graph.END},
		},
	)

	board := graph.NewBoard()
	board.SetVar("route", true)

	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if v := result.GetVarString("result"); v != "took_true" {
		t.Fatalf("expected 'took_true', got %q", v)
	}
}

func TestLocalExecutor_DefaultBranch(t *testing.T) {
	condNever, _ := graph.CompileCondition("x == 999")

	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start":   graph.NewPassthroughNode("start", "passthrough"),
			"special": graph.NewPassthroughNode("special", "passthrough"),
			"default": newTestNode("default", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("branch", "default")
				return nil
			}),
		},
		[]graph.Edge{
			{From: "start", To: "special", Condition: condNever},
			{From: "start", To: "default"},
			{From: "special", To: graph.END},
			{From: "default", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if v := result.GetVarString("branch"); v != "default" {
		t.Fatalf("expected 'default', got %q", v)
	}
}

func TestLocalExecutor_Interrupt_Resume(t *testing.T) {
	var callCount int32

	g := buildGraph("test", "approval",
		map[string]graph.Node{
			"approval": newTestNode("approval", func(_ graph.ExecutionContext, b *graph.Board) error {
				n := atomic.AddInt32(&callCount, 1)
				if n == 1 {
					b.SetVar("approval_status", "pending")
					return graph.ErrInterrupt
				}
				b.SetVar("approval_status", "approved")
				return nil
			}),
			"after": newTestNode("after", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("done", true)
				return nil
			}),
		},
		[]graph.Edge{
			{From: "approval", To: "after"},
			{From: "after", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()

	result, err := exec.Execute(context.Background(), g, board)
	if !errdefs.Is(err, graph.ErrInterrupt) {
		t.Fatalf("expected ErrInterrupt, got %v", err)
	}

	interruptedNode := result.GetVarString(graph.VarInterruptedNode)
	if interruptedNode != "approval" {
		t.Fatalf("expected interrupted_node=approval, got %q", interruptedNode)
	}

	result, err = exec.Execute(context.Background(), g, result,
		WithStartNode(interruptedNode))
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	if v, _ := result.GetVar("done"); v != true {
		t.Fatal("expected done=true after resume")
	}
}

func TestLocalExecutor_SkipCondition(t *testing.T) {
	skipCond, _ := graph.CompileCondition("skip_me == true")

	g := graph.NewGraph(&graph.RawGraph{
		Name:  "test",
		Entry: "start",
		Nodes: map[string]graph.Node{
			"start": newTestNode("start", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("passed_start", true)
				return nil
			}),
			"skippable": newTestNode("skippable", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("was_executed", true)
				return nil
			}),
		},
		Edges: map[string][]graph.Edge{
			"start":     {{From: "start", To: "skippable"}},
			"skippable": {{From: "skippable", To: graph.END}},
		},
		Reverse:        map[string][]string{"skippable": {"start"}, graph.END: {"skippable"}},
		SkipConditions: map[string]*graph.CompiledCondition{"skippable": skipCond},
	}, graph.GraphMeta{})

	board := graph.NewBoard()
	board.SetVar("skip_me", true)

	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if _, ok := result.GetVar("was_executed"); ok {
		t.Fatal("skippable node should not have been executed")
	}
}

func TestLocalExecutor_SkipCondition_FalseDoesNotSkip(t *testing.T) {
	skipCond, _ := graph.CompileCondition("skip_me == true")

	g := graph.NewGraph(&graph.RawGraph{
		Name:  "test",
		Entry: "a",
		Nodes: map[string]graph.Node{
			"a": newTestNode("a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("executed", true)
				return nil
			}),
		},
		Edges: map[string][]graph.Edge{
			"a": {{From: "a", To: graph.END}},
		},
		Reverse:        map[string][]string{graph.END: {"a"}},
		SkipConditions: map[string]*graph.CompiledCondition{"a": skipCond},
	}, graph.GraphMeta{})

	board := graph.NewBoard()
	board.SetVar("skip_me", false)

	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if v, ok := result.GetVar("executed"); !ok || v != true {
		t.Fatal("node should execute when skip condition is false")
	}
}

func TestLocalExecutor_NodeNotFound_Error(t *testing.T) {
	g := buildGraph("test", "missing",
		map[string]graph.Node{},
		[]graph.Edge{},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board)
	if err == nil {
		t.Fatal("expected error for missing node")
	}
}

func TestLocalExecutor_MaxIterations_ExhaustedReturnsError(t *testing.T) {
	loopCond, _ := graph.CompileCondition("loop == true")

	g := buildGraph("test", "a",
		map[string]graph.Node{
			"a": newTestNode("a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("loop", true)
				return nil
			}),
			"b": newTestNode("b", nil),
		},
		[]graph.Edge{
			{From: "a", To: "b", Condition: loopCond},
			{From: "a", To: graph.END},
			{From: "b", To: "a"},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board, WithMaxIterations(5))
	if err == nil {
		t.Fatal("expected error when max iterations exhausted")
	}
}

func TestLocalExecutor_Timeout(t *testing.T) {
	g := buildGraph("test", "slow",
		map[string]graph.Node{
			"slow": newTestNode("slow", func(ctx graph.ExecutionContext, b *graph.Board) error {
				select {
				case <-time.After(5 * time.Second):
					return nil
				case <-ctx.Context.Done():
					return ctx.Context.Err()
				}
			}),
		},
		[]graph.Edge{
			{From: "slow", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board,
		WithTimeout(50*time.Millisecond))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errdefs.IsTimeout(err) {
		t.Fatalf("expected Timeout error, got %v", err)
	}
}

func TestLocalExecutor_AbortDuringNode(t *testing.T) {
	started := make(chan struct{})
	g := buildGraph("test", "blocking",
		map[string]graph.Node{
			"blocking": newTestNode("blocking", func(ctx graph.ExecutionContext, b *graph.Board) error {
				close(started)
				<-ctx.Context.Done()
				return ctx.Context.Err()
			}),
		},
		[]graph.Edge{
			{From: "blocking", To: graph.END},
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	board := graph.NewBoard()
	exec := NewLocalExecutor()

	errCh := make(chan error, 1)
	go func() {
		_, err := exec.Execute(ctx, g, board)
		errCh <- err
	}()

	<-started
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error after abort")
	}
	if !errdefs.IsAborted(err) {
		t.Fatalf("expected Aborted error, got %v", err)
	}
}

func TestLocalExecutor_AbortBetweenNodes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := buildGraph("test", "a",
		map[string]graph.Node{
			"a": newTestNode("a", func(_ graph.ExecutionContext, b *graph.Board) error {
				cancel()
				return nil
			}),
			"b": newTestNode("b", func(_ graph.ExecutionContext, b *graph.Board) error {
				t.Fatal("node b should not execute after abort")
				return nil
			}),
		},
		[]graph.Edge{
			{From: "a", To: "b"},
			{From: "b", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err := exec.Execute(ctx, g, board)
	if err == nil {
		t.Fatal("expected error after abort")
	}
	if !errdefs.IsAborted(err) {
		t.Fatalf("expected Aborted error, got %v", err)
	}
}

func TestLocalExecutor_EventBus_Integration(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx := context.Background()
	const runID = "rint-1"
	sub, err := bus.Subscribe(ctx, PatternRun(runID))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err = exec.Execute(ctx, g, board, WithEventBus(bus), WithRunID(runID))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	wantStart := subjGraphStart(runID)
	wantEnd := subjGraphEnd(runID)

	var envelopes []event.Envelope
	timeout := time.After(time.Second)
loop:
	for {
		select {
		case env, ok := <-sub.C():
			if !ok {
				break loop
			}
			envelopes = append(envelopes, env)
			if env.Subject == wantEnd {
				break loop
			}
		case <-timeout:
			break loop
		}
	}

	if len(envelopes) < 2 {
		t.Fatalf("expected at least 2 envelopes (start+end), got %d", len(envelopes))
	}
	if envelopes[0].Subject != wantStart {
		t.Fatalf("first envelope should be %s, got %s", wantStart, envelopes[0].Subject)
	}
	// Headers must carry the run id for downstream predicate filters.
	if envelopes[0].RunID() != runID {
		t.Fatalf("envelope missing run_id header, got %q", envelopes[0].RunID())
	}
}

func TestLocalExecutor_Compiler_Integration(t *testing.T) {
	c := compiler.NewCompiler()
	def := &graph.GraphDefinition{
		Name:  "integration_test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough"},
			{ID: "middle", Type: "passthrough"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: "middle"},
			{From: "middle", To: graph.END},
		},
	}

	compiled, err := c.Compile(def)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	g := graph.NewGraph(compiled.Graph, compiled.Metadata)

	board := graph.NewBoard()
	board.SetVar("query", "test input")

	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if v := result.GetVarString("query"); v != "test input" {
		t.Fatalf("expected 'test input', got %q", v)
	}
}

func TestLocalExecutor_ConfigRestore_AfterVariableResolution(t *testing.T) {
	origConfig := map[string]any{
		"prompt": "${board.query}",
		"model":  "openai/gpt-4o",
	}

	node := &configurableTestNode{
		id:     "node1",
		config: copyMap(origConfig),
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			if cfg["prompt"] != "hello world" {
				t.Fatalf("expected resolved prompt, got %q", cfg["prompt"])
			}
			b.SetVar("done", true)
			return nil
		},
	}

	g := graph.NewGraph(&graph.RawGraph{
		Name:  "test",
		Entry: "node1",
		Nodes: map[string]graph.Node{"node1": node},
		Edges: map[string][]graph.Edge{
			"node1": {{From: "node1", To: graph.END}},
		},
		Reverse:        map[string][]string{graph.END: {"node1"}},
		SkipConditions: make(map[string]*graph.CompiledCondition),
	}, graph.GraphMeta{})

	board := graph.NewBoard()
	board.SetVar("query", "hello world")

	resolver := variable.NewResolver()
	resolver.AddScope("board", board.Vars())

	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board, WithResolver(resolver))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if node.config["prompt"] != "${board.query}" {
		t.Fatalf("original config should be restored, got prompt=%q", node.config["prompt"])
	}
}

func TestLocalExecutor_TypedVariableResolution(t *testing.T) {
	var capturedConfig map[string]any

	node := &configurableTestNode{
		id: "llm1",
		config: map[string]any{
			"system_prompt": "${board.system_prompt}",
			"temperature":   "${board.temperature}",
			"max_tokens":    "${board.max_tokens}",
			"json_mode":     "${board.json_mode}",
			"model":         "openai/gpt-4o",
		},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			capturedConfig = copyMap(cfg)
			b.SetVar("done", true)
			return nil
		},
	}

	g := graph.NewGraph(&graph.RawGraph{
		Name:  "test",
		Entry: "llm1",
		Nodes: map[string]graph.Node{"llm1": node},
		Edges: map[string][]graph.Edge{
			"llm1": {{From: "llm1", To: graph.END}},
		},
		Reverse:        map[string][]string{graph.END: {"llm1"}},
		SkipConditions: make(map[string]*graph.CompiledCondition),
	}, graph.GraphMeta{})

	board := graph.NewBoard()
	board.SetVar("system_prompt", "You are a translator.")
	board.SetVar("temperature", 0.3)
	board.SetVar("max_tokens", 2048)
	board.SetVar("json_mode", true)

	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board, WithResolver(variable.NewResolver()))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if s, ok := capturedConfig["system_prompt"].(string); !ok || s != "You are a translator." {
		t.Fatalf("system_prompt: got %T %v", capturedConfig["system_prompt"], capturedConfig["system_prompt"])
	}
	if temp, ok := capturedConfig["temperature"].(float64); !ok || temp != 0.3 {
		t.Fatalf("temperature: got %T %v", capturedConfig["temperature"], capturedConfig["temperature"])
	}
	if mt, ok := capturedConfig["max_tokens"].(int); !ok || mt != 2048 {
		t.Fatalf("max_tokens: got %T %v", capturedConfig["max_tokens"], capturedConfig["max_tokens"])
	}
	if jm, ok := capturedConfig["json_mode"].(bool); !ok || !jm {
		t.Fatalf("json_mode: got %T %v", capturedConfig["json_mode"], capturedConfig["json_mode"])
	}
	if node.config["temperature"] != "${board.temperature}" {
		t.Fatalf("original config should be restored, got temperature=%v", node.config["temperature"])
	}
}

func TestLocalExecutor_TypedResolution_UnresolvedFallback(t *testing.T) {
	var capturedConfig map[string]any

	node := &configurableTestNode{
		id: "n1",
		config: map[string]any{
			"temperature": "${board.missing_temp}",
			"prompt":      "prefix ${board.missing_suffix}",
		},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			capturedConfig = copyMap(cfg)
			b.SetVar("done", true)
			return nil
		},
	}

	g := graph.NewGraph(&graph.RawGraph{
		Name:  "test",
		Entry: "n1",
		Nodes: map[string]graph.Node{"n1": node},
		Edges: map[string][]graph.Edge{
			"n1": {{From: "n1", To: graph.END}},
		},
		Reverse:        map[string][]string{graph.END: {"n1"}},
		SkipConditions: make(map[string]*graph.CompiledCondition),
	}, graph.GraphMeta{})

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board, WithResolver(variable.NewResolver()))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if s, ok := capturedConfig["temperature"].(string); !ok || s != "${board.missing_temp}" {
		t.Fatalf("unresolved temperature: got %T %v", capturedConfig["temperature"], capturedConfig["temperature"])
	}
	if s, ok := capturedConfig["prompt"].(string); !ok || s != "prefix ${board.missing_suffix}" {
		t.Fatalf("unresolved prompt: got %T %v", capturedConfig["prompt"], capturedConfig["prompt"])
	}
}

func TestLocalExecutor_TemplateRef_BuildThenExecute(t *testing.T) {
	var capturedConfig map[string]any

	node := &configurableTestNode{
		id: "llm1",
		config: map[string]any{
			"system_prompt": "${board.system_prompt}",
			"temperature":   "${board.temperature}",
			"max_tokens":    "${board.max_tokens}",
			"json_mode":     "${board.json_mode}",
			"model":         "openai/gpt-4o",
		},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			capturedConfig = copyMap(cfg)
			b.SetVar("done", true)
			return nil
		},
	}

	origConfig := copyMap(node.config)

	g := graph.NewGraph(&graph.RawGraph{
		Name:  "test",
		Entry: "llm1",
		Nodes: map[string]graph.Node{"llm1": node},
		Edges: map[string][]graph.Edge{
			"llm1": {{From: "llm1", To: graph.END}},
		},
		Reverse:        map[string][]string{graph.END: {"llm1"}},
		SkipConditions: make(map[string]*graph.CompiledCondition),
	}, graph.GraphMeta{})

	board := graph.NewBoard()
	board.SetVar("system_prompt", "You are a translator.")
	board.SetVar("temperature", 0.3)
	board.SetVar("max_tokens", 2048)
	board.SetVar("json_mode", true)

	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board, WithResolver(variable.NewResolver()))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	// Verify resolved values reached the node during execution.
	if s, ok := capturedConfig["system_prompt"].(string); !ok || s != "You are a translator." {
		t.Fatalf("system_prompt: got %T %v", capturedConfig["system_prompt"], capturedConfig["system_prompt"])
	}
	if temp, ok := capturedConfig["temperature"].(float64); !ok || temp != 0.3 {
		t.Fatalf("temperature: got %T %v", capturedConfig["temperature"], capturedConfig["temperature"])
	}
	if mt, ok := capturedConfig["max_tokens"].(int); !ok || mt != 2048 {
		t.Fatalf("max_tokens: got %T %v", capturedConfig["max_tokens"], capturedConfig["max_tokens"])
	}
	if jm, ok := capturedConfig["json_mode"].(bool); !ok || !jm {
		t.Fatalf("json_mode: got %T %v", capturedConfig["json_mode"], capturedConfig["json_mode"])
	}

	// Verify original config is restored after execution.
	for k, v := range origConfig {
		if node.config[k] != v {
			t.Fatalf("config[%q] not restored: got %v, want %v", k, node.config[k], v)
		}
	}
}

func TestLocalExecutor_TemplateRef_StringNumericBoardVars(t *testing.T) {
	var capturedConfig map[string]any

	node := &configurableTestNode{
		id: "n1",
		config: map[string]any{
			"temperature": "${board.temperature}",
			"max_tokens":  "${board.max_tokens}",
			"json_mode":   "${board.json_mode}",
		},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			capturedConfig = copyMap(cfg)
			b.SetVar("done", true)
			return nil
		},
	}

	g := graph.NewGraph(&graph.RawGraph{
		Name:  "test",
		Entry: "n1",
		Nodes: map[string]graph.Node{"n1": node},
		Edges: map[string][]graph.Edge{
			"n1": {{From: "n1", To: graph.END}},
		},
		Reverse:        map[string][]string{graph.END: {"n1"}},
		SkipConditions: make(map[string]*graph.CompiledCondition),
	}, graph.GraphMeta{})

	board := graph.NewBoard()
	board.SetVar("temperature", "0.7")
	board.SetVar("max_tokens", "4096")
	board.SetVar("json_mode", "true")

	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board, WithResolver(variable.NewResolver()))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	// When board vars are strings, resolveTyped returns the string value.
	// The node's SetConfig → CoerceMapForStruct should still handle them.
	if capturedConfig["temperature"] != "0.7" {
		t.Fatalf("temperature: got %T %v", capturedConfig["temperature"], capturedConfig["temperature"])
	}
	if capturedConfig["max_tokens"] != "4096" {
		t.Fatalf("max_tokens: got %T %v", capturedConfig["max_tokens"], capturedConfig["max_tokens"])
	}
	if capturedConfig["json_mode"] != "true" {
		t.Fatalf("json_mode: got %T %v", capturedConfig["json_mode"], capturedConfig["json_mode"])
	}
}

func TestLocalExecutor_PartialTemplateRef_PreservesLiteral(t *testing.T) {
	var capturedConfig map[string]any

	node := &configurableTestNode{
		id: "n1",
		config: map[string]any{
			"system_prompt": "Translate: ${board.lang}",
			"temperature":   float64(0.5),
			"model":         "${board.model}",
		},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			capturedConfig = copyMap(cfg)
			return nil
		},
	}

	g := graph.NewGraph(&graph.RawGraph{
		Name:  "test",
		Entry: "n1",
		Nodes: map[string]graph.Node{"n1": node},
		Edges: map[string][]graph.Edge{
			"n1": {{From: "n1", To: graph.END}},
		},
		Reverse:        map[string][]string{graph.END: {"n1"}},
		SkipConditions: make(map[string]*graph.CompiledCondition),
	}, graph.GraphMeta{})

	board := graph.NewBoard()
	board.SetVar("lang", "Chinese")
	board.SetVar("model", "openai/gpt-4o")

	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board, WithResolver(variable.NewResolver()))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if s, ok := capturedConfig["system_prompt"].(string); !ok || s != "Translate: Chinese" {
		t.Fatalf("system_prompt: got %T %v", capturedConfig["system_prompt"], capturedConfig["system_prompt"])
	}
	if temp, ok := capturedConfig["temperature"].(float64); !ok || temp != 0.5 {
		t.Fatalf("temperature: got %T %v, want 0.5", capturedConfig["temperature"], capturedConfig["temperature"])
	}
	if s, ok := capturedConfig["model"].(string); !ok || s != "openai/gpt-4o" {
		t.Fatalf("model: got %T %v", capturedConfig["model"], capturedConfig["model"])
	}
}
