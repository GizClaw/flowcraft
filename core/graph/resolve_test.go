package graph

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
)

func TestResolveConfigWholeStringKeepsType(t *testing.T) {
	board := agent.NewBoard()
	board.SetVar("docs", []any{"a", "b"})
	board.SetVar("limit", float64(3))

	raw := json.RawMessage(`{"docs": "${board.docs}", "n": "${board.limit}"}`)
	out, err := resolveConfig(raw, board)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	var decoded struct {
		Docs []any `json:"docs"`
		N    int   `json:"n"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal resolved: %v", err)
	}
	if len(decoded.Docs) != 2 || decoded.N != 3 {
		t.Fatalf("typed values lost: %+v", decoded)
	}
}

func TestResolveConfigInterpolatesEmbeddedRefs(t *testing.T) {
	board := agent.NewBoard()
	board.SetVar("city", "Paris")

	raw := json.RawMessage(`{"prompt": "weather in ${board.city} please"}`)
	out, err := resolveConfig(raw, board)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal resolved: %v", err)
	}
	if decoded["prompt"] != "weather in Paris please" {
		t.Fatalf("interpolation wrong: %q", decoded["prompt"])
	}
}

func TestResolveConfigLeavesMissingVarsUntouched(t *testing.T) {
	board := agent.NewBoard()
	raw := json.RawMessage(`{"prompt": "${board.nope}"}`)
	out, err := resolveConfig(raw, board)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if string(out) != `{"prompt":"${board.nope}"}` && string(out) != string(raw) {
		t.Fatalf("missing var should stay literal, got %s", out)
	}
}

func TestResolveConfigFastPath(t *testing.T) {
	raw := json.RawMessage(`{"a": 1}`)
	out, err := resolveConfig(raw, agent.NewBoard())
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("fast path should return input unchanged, got %s", out)
	}
}

func TestExtractRefs(t *testing.T) {
	cfg := map[string]any{
		"a": "${board.x}",
		"b": []any{"${board.y}", "plain ${board.x}"},
		"c": 3,
	}
	refs := ExtractRefs(cfg)
	if len(refs) != 2 || refs[0] != "x" || refs[1] != "y" {
		t.Fatalf("ExtractRefs = %v", refs)
	}
	if !ContainsRef(cfg["a"]) || ContainsRef(cfg["c"]) {
		t.Fatalf("ContainsRef wrong")
	}
}
