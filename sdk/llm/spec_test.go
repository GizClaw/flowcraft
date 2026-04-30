package llm

import "testing"

func TestModelSpec_IsZero(t *testing.T) {
	cases := []struct {
		name string
		s    ModelSpec
		want bool
	}{
		{"all zero", ModelSpec{}, true},
		{"caps set", ModelSpec{Caps: DisabledCaps(CapTemperature)}, false},
		{"defaults set", ModelSpec{Defaults: GenerateOptions{Temperature: floatPtr(0.7)}}, false},
		{"limits set", ModelSpec{Limits: ModelLimits{MaxOutputTokens: 1000}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.IsZero(); got != c.want {
				t.Fatalf("IsZero=%v, want %v", got, c.want)
			}
		})
	}
}

func TestModelLimits_IsZero(t *testing.T) {
	if !(ModelLimits{}).IsZero() {
		t.Fatal("zero ModelLimits should be IsZero")
	}
	if (ModelLimits{MaxOutputTokens: 1}).IsZero() {
		t.Fatal("MaxOutputTokens=1 should not be IsZero")
	}
}

func TestMergeSpec_FieldwiseOverlay(t *testing.T) {
	base := ModelSpec{
		Caps:     DisabledCaps(CapTemperature),
		Defaults: GenerateOptions{Temperature: floatPtr(0.5)},
		Limits:   ModelLimits{MaxOutputTokens: 1000, MaxToolDefinitions: 8},
	}
	overlay := ModelSpec{
		Caps:     DisabledCaps(CapJSONMode),
		Defaults: GenerateOptions{MaxTokens: int64Ptr(200)},
		Limits:   ModelLimits{MaxOutputTokens: 500}, // stricter wins
	}
	got := mergeSpec(base, overlay)

	// Caps OR-merged.
	if got.Caps.Supports(CapTemperature) {
		t.Error("CapTemperature should remain disabled (from base)")
	}
	if got.Caps.Supports(CapJSONMode) {
		t.Error("CapJSONMode should be disabled (from overlay)")
	}

	// Defaults: base.Temperature preserved (overlay didn't set), overlay.MaxTokens added.
	if got.Defaults.Temperature == nil || *got.Defaults.Temperature != 0.5 {
		t.Errorf("Defaults.Temperature: want 0.5, got %v", got.Defaults.Temperature)
	}
	if got.Defaults.MaxTokens == nil || *got.Defaults.MaxTokens != 200 {
		t.Errorf("Defaults.MaxTokens: want 200, got %v", got.Defaults.MaxTokens)
	}

	// Limits: stricter wins.
	if got.Limits.MaxOutputTokens != 500 {
		t.Errorf("MaxOutputTokens: want 500 (stricter), got %d", got.Limits.MaxOutputTokens)
	}
	// MaxToolDefinitions: only base set, preserved.
	if got.Limits.MaxToolDefinitions != 8 {
		t.Errorf("MaxToolDefinitions: want 8, got %d", got.Limits.MaxToolDefinitions)
	}
}

func TestMergeSpec_StricterLimitWins(t *testing.T) {
	// Overlay declares a LOOSER limit than base — base should win.
	base := ModelSpec{Limits: ModelLimits{MaxOutputTokens: 100}}
	overlay := ModelSpec{Limits: ModelLimits{MaxOutputTokens: 1000}}
	got := mergeSpec(base, overlay)
	if got.Limits.MaxOutputTokens != 100 {
		t.Fatalf("want stricter limit 100, got %d", got.Limits.MaxOutputTokens)
	}
}

func TestMergeOptions_OverlayWinsButNeverClears(t *testing.T) {
	base := GenerateOptions{Temperature: floatPtr(0.5), MaxTokens: int64Ptr(100)}
	overlay := GenerateOptions{Temperature: floatPtr(0.9)} // MaxTokens stays
	got := mergeOptions(base, overlay)
	if *got.Temperature != 0.9 {
		t.Errorf("overlay Temperature should win: got %v", *got.Temperature)
	}
	if *got.MaxTokens != 100 {
		t.Errorf("base MaxTokens should be preserved when overlay leaves nil: got %v", *got.MaxTokens)
	}
}

func TestMergeOptions_ExtraIsKeyMerged(t *testing.T) {
	base := GenerateOptions{Extra: map[string]any{"a": 1, "b": 2}}
	overlay := GenerateOptions{Extra: map[string]any{"b": 99, "c": 3}}
	got := mergeOptions(base, overlay)
	if got.Extra["a"] != 1 || got.Extra["b"] != 99 || got.Extra["c"] != 3 {
		t.Fatalf("Extra merge wrong: %v", got.Extra)
	}
}

func TestMergeCaps_OrSemantics(t *testing.T) {
	a := DisabledCaps(CapTemperature)
	b := DisabledCaps(CapJSONSchema)
	merged := mergeCaps(a, b)
	if merged.Supports(CapTemperature) || merged.Supports(CapJSONSchema) {
		t.Fatalf("disabled caps from any layer should remain disabled: %+v", merged)
	}
	if !merged.Supports(CapJSONMode) {
		t.Fatalf("untouched caps should remain supported: %+v", merged)
	}
}

func floatPtr(f float64) *float64 { return &f }
func int64Ptr(i int64) *int64     { return &i }
