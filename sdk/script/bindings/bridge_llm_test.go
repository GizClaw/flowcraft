package bindings

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

func pf(v float64) *float64 { return &v }

func TestMergeRoundConfig(t *testing.T) {
	base := llm.RoundConfig{
		Model:       "m1",
		Temperature: pf(0.5),
		MaxTokens:   100,
	}
	out := mergeRoundConfig(base, map[string]any{"temperature": 0.1, "json_mode": true})
	if out.Model != "m1" {
		t.Fatalf("model = %q", out.Model)
	}
	if out.Temperature == nil || *out.Temperature != 0.1 {
		t.Fatalf("temperature = %v", out.Temperature)
	}
	if out.MaxTokens != 100 {
		t.Fatalf("max tokens = %v", out.MaxTokens)
	}
	if !out.JSONMode {
		t.Fatal("json_mode not merged")
	}
}

func TestMergeRoundConfig_NilOverrides(t *testing.T) {
	base := llm.RoundConfig{Model: "m1"}
	out := mergeRoundConfig(base, nil)
	if out.Model != "m1" {
		t.Fatalf("model = %q", out.Model)
	}
}

func TestNormalizeLLMOverrides_Nil(t *testing.T) {
	if got := normalizeLLMOverrides(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestNormalizeLLMOverrides_Map(t *testing.T) {
	m := map[string]any{"model": "x"}
	got := normalizeLLMOverrides(m)
	if got["model"] != "x" {
		t.Fatalf("expected map passthrough")
	}
}

func TestNormalizeLLMOverrides_NonMap(t *testing.T) {
	if got := normalizeLLMOverrides("string"); got != nil {
		t.Fatalf("expected nil for non-map, got %v", got)
	}
}
