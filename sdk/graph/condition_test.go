package graph

import "testing"

func TestCompileCondition_Valid(t *testing.T) {
	cond, err := CompileCondition("x > 3")
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if cond.Raw != "x > 3" {
		t.Fatalf("raw mismatch: %s", cond.Raw)
	}
}

func TestCompileCondition_Invalid(t *testing.T) {
	_, err := CompileCondition("x >>>> 3")
	if err == nil {
		t.Fatal("expected compile error")
	}
}

func TestCompiledCondition_Evaluate(t *testing.T) {
	cond, err := CompileCondition("x > 3")
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	b := NewBoard()
	b.SetVar("x", 5)
	result, err := cond.Evaluate(b)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	if !result {
		t.Fatal("expected true for 5 > 3")
	}

	b.SetVar("x", 2)
	result, err = cond.Evaluate(b)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	if result {
		t.Fatal("expected false for 2 > 3")
	}
}

func TestCompiledCondition_BooleanLiteral(t *testing.T) {
	cond, _ := CompileCondition("tool_pending == true")
	b := NewBoard()
	b.SetVar("tool_pending", true)

	result, err := cond.Evaluate(b)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	if !result {
		t.Fatal("expected true")
	}
}

func TestCompiledCondition_StringComparison(t *testing.T) {
	cond, _ := CompileCondition(`route_target == "research"`)
	b := NewBoard()
	b.SetVar("route_target", "research")

	result, err := cond.Evaluate(b)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	if !result {
		t.Fatal("expected true")
	}
}
