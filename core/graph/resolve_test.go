package graph

import (
	"bytes"
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

func TestResolveConfigMissingVarFails(t *testing.T) {
	board := agent.NewBoard()
	raw := json.RawMessage(`{"prompt": "${board.nope}"}`)
	if _, err := resolveConfig(raw, board); err == nil {
		t.Fatal("resolveConfig should fail on a missing variable")
	}
}

func TestResolveConfigDotPathAndDefault(t *testing.T) {
	board := agent.NewBoard()
	board.SetVar("user", map[string]any{"name": "ada"})

	raw := json.RawMessage(`{"prompt": "hi ${board.user.name} (${board.fallback:anon})"}`)
	out, err := resolveConfig(raw, board)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	var decoded struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal resolved: %v", err)
	}
	if decoded.Prompt != "hi ada (anon)" {
		t.Fatalf("prompt = %q", decoded.Prompt)
	}
}

func TestResolveConfigEscapedRefStaysLiteral(t *testing.T) {
	board := agent.NewBoard()
	board.SetVar("x", "Paris")

	raw := json.RawMessage(`{"prompt": "\\${board.x}"}`)
	out, err := resolveConfig(raw, board)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	var decoded struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal resolved: %v", err)
	}
	if decoded.Prompt != "${board.x}" {
		t.Fatalf("prompt = %q, want literal ${board.x}", decoded.Prompt)
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

func TestResolveConfigPreservesBigIntegers(t *testing.T) {
	board := agent.NewBoard()
	board.SetVar("x", "ok")

	raw := json.RawMessage(
		`{"id": 9007199254740993, "note": "see ${board.x} docs", "wide": 123456789012345678901234567890}`)
	out, err := resolveConfig(raw, board)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	// The unrelated literals must survive the round-trip verbatim, not
	// as float64 re-encodings (which would lose precision).
	if !bytes.Contains(out, []byte("9007199254740993")) {
		t.Fatalf("int64 literal lost: %s", out)
	}
	if !bytes.Contains(out, []byte("123456789012345678901234567890")) {
		t.Fatalf("wide integer literal lost: %s", out)
	}

	var decoded struct {
		ID   int64  `json:"id"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal resolved: %v", err)
	}
	if decoded.ID != 9007199254740993 || decoded.Note != "see ok docs" {
		t.Fatalf("decoded = %+v", decoded)
	}
	var wide struct {
		Wide json.Number `json:"wide"`
	}
	if err := json.Unmarshal(out, &wide); err != nil {
		t.Fatalf("unmarshal wide: %v", err)
	}
	if wide.Wide.String() != "123456789012345678901234567890" {
		t.Fatalf("wide = %s", wide.Wide.String())
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
