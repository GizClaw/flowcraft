package qwen

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestDashUsageCanonical(t *testing.T) {
	usage := dashUsage{
		InputTokens:     11,
		OutputTokens:    3,
		TotalTokens:     14,
		CachedTokens:    2,
		ReasoningTokens: 5,
	}
	canonical := usage.canonical()
	if canonical.Input.CacheReadTokens == nil ||
		*canonical.Input.CacheReadTokens != 2 {
		t.Fatalf("cache read = %+v", canonical.Input)
	}
	if canonical.Output.ReasoningTokens == nil ||
		*canonical.Output.ReasoningTokens != 5 {
		t.Fatalf("reasoning = %+v", canonical.Output)
	}
	if canonical.Output.ReasoningAccounting != inference.ReasoningAdditional {
		t.Fatalf(
			"reasoning accounting = %q, want %q",
			canonical.Output.ReasoningAccounting,
			inference.ReasoningAdditional,
		)
	}
	// Reasoning rides outside output_tokens on DashScope, so the canonical
	// usage must pass validation with the additional accounting semantics.
	if err := canonical.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDashResponseUsageParsesCacheAndReasoning(t *testing.T) {
	var envelope dashResponse
	if err := json.Unmarshal([]byte(`{
		"usage": {
			"input_tokens": 11,
			"output_tokens": 3,
			"total_tokens": 14,
			"input_tokens_details": {"cached_tokens": 2},
			"output_tokens_details": {"reasoning_tokens": 5}
		}
	}`), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	usage := envelope.usage()
	if usage.CachedTokens != 2 {
		t.Fatalf("cached tokens = %d, want 2", usage.CachedTokens)
	}
	if usage.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want 5", usage.ReasoningTokens)
	}
}
