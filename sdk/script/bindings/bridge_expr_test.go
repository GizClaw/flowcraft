package bindings_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
)

func TestExprBridge(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := engine.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board), bindings.NewExprBridge())
	_, err := rt.Exec(context.Background(), "expr", `
		var result = expr.eval("a + b", {a: 3, b: 4});
		if (result !== 7) throw new Error("expected 7, got " + result);
		board.setVar("expr_result", result);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, _ := board.GetVar("expr_result")
	if v != int64(7) {
		t.Fatalf("expr_result = %v (type %T)", v, v)
	}
}

func TestExprBridge_CachedResults(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := engine.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board), bindings.NewExprBridge())
	_, err := rt.Exec(context.Background(), "expr-cache", `
		var r1 = expr.eval("x * 2", {x: 5});
		var r2 = expr.eval("x * 2", {x: 10});
		if (r1 !== 10) throw new Error("first eval: " + r1);
		if (r2 !== 20) throw new Error("second eval (same expr, diff env): " + r2);
		board.setVar("cache_ok", true);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := board.GetVar("cache_ok")
	if v != true {
		t.Fatal("cached expr results should be correct")
	}
}

func TestExprBridge_InvalidExpression(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := engine.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board), bindings.NewExprBridge())
	_, err := rt.Exec(context.Background(), "expr-invalid", `
		try {
			expr.eval("!!!invalid", {});
			throw new Error("should have failed");
		} catch(e) {
			board.setVar("caught", true);
		}
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := board.GetVar("caught")
	if v != true {
		t.Fatal("invalid expression should throw error")
	}
}
