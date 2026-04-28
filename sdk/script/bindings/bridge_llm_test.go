package bindings

import (
	"reflect"
	"strings"
	"testing"
)

func pf(v float64) *float64 { return &v }
func pb(v bool) *bool       { return &v }

// ---------------------------------------------------------------------------
// parseRunOptions — input validation
// ---------------------------------------------------------------------------

func TestParseRunOptions_Nil(t *testing.T) {
	got, err := parseRunOptions(nil)
	if err != nil {
		t.Fatalf("nil should be accepted, got err: %v", err)
	}
	if !reflect.DeepEqual(got, LLMRunOptions{}) {
		t.Fatalf("nil should yield zero options, got %+v", got)
	}
}

func TestParseRunOptions_EmptyMap(t *testing.T) {
	got, err := parseRunOptions(map[string]any{})
	if err != nil {
		t.Fatalf("empty map should be accepted, got err: %v", err)
	}
	if !reflect.DeepEqual(got, LLMRunOptions{}) {
		t.Fatalf("empty map should yield zero options, got %+v", got)
	}
}

func TestParseRunOptions_NonMap(t *testing.T) {
	cases := []any{
		"a string",
		42,
		3.14,
		true,
		[]any{1, 2, 3},
	}
	for _, v := range cases {
		_, err := parseRunOptions(v)
		if err == nil {
			t.Fatalf("non-map input %T should be rejected", v)
		}
		if !strings.Contains(err.Error(), "must be an object") {
			t.Fatalf("error should explain expected object shape, got: %v", err)
		}
	}
}

func TestParseRunOptions_AllFields(t *testing.T) {
	in := map[string]any{
		"model":       "openai/gpt-4o-mini",
		"temperature": 0.25,
		"max_tokens":  float64(1024), // jsrt/luart deliver numbers as float64
		"json_mode":   true,
		"thinking":    false,
		"tools":       []any{"web_search", "calc"},
	}
	got, err := parseRunOptions(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "openai/gpt-4o-mini" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Temperature == nil || *got.Temperature != 0.25 {
		t.Errorf("temperature = %v", got.Temperature)
	}
	if got.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d", got.MaxTokens)
	}
	if got.JSONMode == nil || *got.JSONMode != true {
		t.Errorf("json_mode = %v", got.JSONMode)
	}
	if got.Thinking == nil || *got.Thinking != false {
		t.Errorf("thinking = %v", got.Thinking)
	}
	if len(got.Tools) != 2 || got.Tools[0] != "web_search" || got.Tools[1] != "calc" {
		t.Errorf("tools = %v", got.Tools)
	}
}

func TestParseRunOptions_UnknownField_Rejected(t *testing.T) {
	_, err := parseRunOptions(map[string]any{
		"model":      "m1",
		"temprature": 0.5, // typo!
		"json_mode":  true,
	})
	if err == nil {
		t.Fatal("unknown field should be rejected (typo protection)")
	}
	if !strings.Contains(err.Error(), "temprature") {
		t.Fatalf("error should name the offending field, got: %v", err)
	}
}

func TestParseRunOptions_TypeMismatch_Rejected(t *testing.T) {
	cases := []map[string]any{
		{"temperature": "hot"}, // string in number field
		{"max_tokens": "lots"},
		{"json_mode": "yes"},
		{"tools": "web_search"}, // string instead of []string
	}
	for _, in := range cases {
		_, err := parseRunOptions(in)
		if err == nil {
			t.Fatalf("type mismatch %v should be rejected", in)
		}
	}
}

// Pointer-bool fields are the only way to express "explicitly false"
// without it being indistinguishable from "field not provided". This
// test guards that decoding paths preserve that distinction.
func TestParseRunOptions_BoolPointers_PreserveExplicitFalse(t *testing.T) {
	got, err := parseRunOptions(map[string]any{
		"json_mode": false,
		"thinking":  false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.JSONMode == nil {
		t.Fatal("json_mode=false should produce non-nil pointer")
	}
	if *got.JSONMode != false {
		t.Errorf("json_mode value = %v", *got.JSONMode)
	}
	if got.Thinking == nil {
		t.Fatal("thinking=false should produce non-nil pointer")
	}
	if *got.Thinking != false {
		t.Errorf("thinking value = %v", *got.Thinking)
	}
}

// ---------------------------------------------------------------------------
// toRoundOptions — defaults × override merge semantics
// ---------------------------------------------------------------------------

func TestToRoundOptions_EmptyOverride_ReturnsDefaults(t *testing.T) {
	defaults := LLMRunOptions{
		Model:       "base-model",
		Temperature: pf(0.5),
		MaxTokens:   2048,
		JSONMode:    pb(true),
		Thinking:    pb(true),
		Tools:       []string{"tool_a"},
	}
	got := toRoundOptions(defaults, LLMRunOptions{})
	if got.Model != "base-model" {
		t.Errorf("model: got %q", got.Model)
	}
	if got.Temperature != defaults.Temperature {
		t.Error("temperature pointer should be inherited verbatim")
	}
	if got.MaxTokens != 2048 {
		t.Errorf("max_tokens: got %d", got.MaxTokens)
	}
	if !got.JSONMode || !got.Thinking {
		t.Errorf("bool flags lost: %+v", got)
	}
	if len(got.ToolNames) != 1 || got.ToolNames[0] != "tool_a" {
		t.Errorf("tools: got %v", got.ToolNames)
	}
}

func TestToRoundOptions_PartialOverride_ModelOnly(t *testing.T) {
	defaults := LLMRunOptions{Model: "base", Temperature: pf(0.5), MaxTokens: 100}
	got := toRoundOptions(defaults, LLMRunOptions{Model: "override"})
	if got.Model != "override" {
		t.Errorf("model not overridden: %q", got.Model)
	}
	if got.Temperature != defaults.Temperature {
		t.Errorf("temperature should inherit, got %v", got.Temperature)
	}
	if got.MaxTokens != 100 {
		t.Errorf("max_tokens should inherit, got %d", got.MaxTokens)
	}
}

func TestToRoundOptions_FullOverride(t *testing.T) {
	defaults := LLMRunOptions{
		Model: "base-m", Temperature: pf(0.5), MaxTokens: 100,
		JSONMode: pb(false), Thinking: pb(false), Tools: []string{"old"},
	}
	got := toRoundOptions(defaults, LLMRunOptions{
		Model:       "new-m",
		Temperature: pf(0.1),
		MaxTokens:   2048,
		JSONMode:    pb(true),
		Thinking:    pb(true),
		Tools:       []string{"new"},
	})
	if got.Model != "new-m" || *got.Temperature != 0.1 || got.MaxTokens != 2048 {
		t.Errorf("scalar overrides failed: %+v", got)
	}
	if !got.JSONMode || !got.Thinking {
		t.Errorf("bool overrides failed: %+v", got)
	}
	if len(got.ToolNames) != 1 || got.ToolNames[0] != "new" {
		t.Errorf("tools override failed: %v", got.ToolNames)
	}
}

// JSONMode/Thinking *bool semantics: explicit false in the override
// MUST flip a defaults value of true to false. Earlier "if !zero"
// merge strategies silently dropped this case.
func TestToRoundOptions_BoolPointer_FalseClearsDefaultsTrue(t *testing.T) {
	defaults := LLMRunOptions{JSONMode: pb(true), Thinking: pb(true)}
	got := toRoundOptions(defaults, LLMRunOptions{
		JSONMode: pb(false),
		Thinking: pb(false),
	})
	if got.JSONMode || got.Thinking {
		t.Errorf("explicit false should disable defaults-true, got %+v", got)
	}
}

func TestToRoundOptions_DoesNotMutateDefaults(t *testing.T) {
	temp := 0.5
	defaults := LLMRunOptions{
		Model: "base", Temperature: &temp, MaxTokens: 100,
		Tools: []string{"t1"},
	}
	_ = toRoundOptions(defaults, LLMRunOptions{
		Model:       "other",
		Temperature: pf(0.9),
		Tools:       []string{"t2"},
	})
	if defaults.Model != "base" {
		t.Errorf("defaults.Model mutated: %q", defaults.Model)
	}
	if *defaults.Temperature != 0.5 {
		t.Errorf("defaults.Temperature mutated: %v", *defaults.Temperature)
	}
	if len(defaults.Tools) != 1 || defaults.Tools[0] != "t1" {
		t.Errorf("defaults.Tools mutated: %v", defaults.Tools)
	}
}

// Tools merge is REPLACE semantics (decision #5). A script supplying
// any non-empty Tools list must overwrite the defaults entirely; this
// keeps script intent explicit and avoids surprise additive behavior.
func TestToRoundOptions_Tools_ReplaceNotAppend(t *testing.T) {
	defaults := LLMRunOptions{Tools: []string{"a", "b", "c"}}
	got := toRoundOptions(defaults, LLMRunOptions{Tools: []string{"x"}})
	if len(got.ToolNames) != 1 || got.ToolNames[0] != "x" {
		t.Errorf("tools should be replaced, got %v", got.ToolNames)
	}
}

func TestToRoundOptions_Tools_EmptyOverride_KeepsDefaults(t *testing.T) {
	defaults := LLMRunOptions{Tools: []string{"a"}}
	got := toRoundOptions(defaults, LLMRunOptions{}) // no Tools field
	if len(got.ToolNames) != 1 || got.ToolNames[0] != "a" {
		t.Errorf("empty override should keep defaults: %v", got.ToolNames)
	}
}
