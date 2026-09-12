package anthropic

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// scopedEntry returns the entry the opener hands to the compiler: the catalog
// entry with the verification scope of one addressed model filled in.
func scopedEntry(entry catalogEntry, provider, name, profile string) catalogEntry {
	entry.reasoningScope = inference.ReasoningScope(
		entry.reasoningScopeDeclared, provider, name, profile)
	return entry
}

// decisionFor returns the report entry for one field.
func decisionFor(
	decisions []inference.Decision,
	field inference.FieldID,
) (inference.Decision, bool) {
	for _, decision := range decisions {
		if decision.Field == field {
			return decision, true
		}
	}
	return inference.Decision{}, false
}

// reasoningHistoryRequest puts one reasoning trace in assistant history.
func reasoningHistoryRequest(trace message.ReasoningPart) inference.GenerateRequest {
	request := conformanceTextRequest()
	request.Context = []message.Message{{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			trace,
			message.TextPart{Text: "answer"},
		}},
	}}
	return request
}

// TestForeignReasoningSignatureIsDroppedNotSent covers the cross-provider
// failure #527 reports: an OpenAI reasoning item carries a signature slot too,
// so a shape-only gate forwards the encrypted payload as an Anthropic
// thinking signature and the endpoint rejects the whole request with a 400
// that no retry can repair. Provenance turns that into a reported drop.
func TestForeignReasoningSignatureIsDroppedNotSent(t *testing.T) {
	entry := scopedEntry(
		catalog["claude-fable-5"], "minimax", "minimax-m3", "default")
	compiled, err := compileGenerate("minimax-m3", entry)(
		context.Background(),
		model.ModelRef{
			ID:      model.ModelID{Provider: "minimax", Name: "minimax-m3"},
			Profile: "default",
		},
		reasoningHistoryRequest(message.ReasoningPart{
			Text:      "openai summary",
			Signature: "enc-openai",
			ID:        "rs_openai",
			Source:    "openai/gpt-5.6-luna/default",
		}),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	decision, ok := decisionFor(
		compiled.Report.Decisions, inference.FieldGenerateContextReasoning)
	if !ok {
		t.Fatalf("no reasoning decision: %+v", compiled.Report.Decisions)
	}
	if decision.Disposition != inference.Dropped {
		t.Fatalf("decision = %+v, want Dropped", decision)
	}
	if !strings.Contains(decision.Reason, "openai/gpt-5.6-luna/default") {
		t.Fatalf("reason = %q, want the producing scope named", decision.Reason)
	}
	for _, block := range compiled.Wire.Messages {
		for _, content := range block.Content {
			if content.OfThinking != nil || content.OfRedactedThinking != nil {
				t.Fatalf("foreign reasoning reached the request: %+v", content)
			}
		}
	}
}

// TestOwnReasoningSignatureIsSent pins the other side: a trace this
// deployment produced still round-trips as a thinking block.
func TestOwnReasoningSignatureIsSent(t *testing.T) {
	entry := scopedEntry(
		catalog["claude-fable-5"], "anthropic", "claude-fable-5", "default")
	compiled, err := compileGenerate("claude-fable-5", entry)(
		context.Background(),
		model.ModelRef{
			ID:      model.ModelID{Provider: "anthropic", Name: "claude-fable-5"},
			Profile: "default",
		},
		reasoningHistoryRequest(message.ReasoningPart{
			Text:      "own thinking",
			Signature: "sig-own",
			Source:    "anthropic/claude-fable-5/default",
		}),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	decision, ok := decisionFor(
		compiled.Report.Decisions, inference.FieldGenerateContextReasoning)
	if !ok || decision.Disposition != inference.Native {
		t.Fatalf("decision = %+v, want Native", decision)
	}
}

// TestReasoningWithoutProvenanceIsDropped covers transcripts stored before
// drivers stamped their traces.
func TestReasoningWithoutProvenanceIsDropped(t *testing.T) {
	entry := scopedEntry(
		catalog["claude-fable-5"], "anthropic", "claude-fable-5", "default")
	compiled, err := compileGenerate("claude-fable-5", entry)(
		context.Background(),
		model.ModelRef{
			ID:      model.ModelID{Provider: "anthropic", Name: "claude-fable-5"},
			Profile: "default",
		},
		reasoningHistoryRequest(message.ReasoningPart{
			Text:      "legacy thinking",
			Signature: "sig-legacy",
		}),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	decision, ok := decisionFor(
		compiled.Report.Decisions, inference.FieldGenerateContextReasoning)
	if !ok || decision.Disposition != inference.Dropped ||
		!strings.Contains(decision.Reason, "no provenance") {
		t.Fatalf("decision = %+v, want a provenance drop", decision)
	}
}

// TestDeclaredReasoningScopeSharesTraces pins the escape hatch for a
// compatible endpoint that verifies the same signatures across models.
func TestDeclaredReasoningScopeSharesTraces(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"wire":{"reasoning_scope":"gateway-shared"}}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := scopedEntry(models["claude-fable-5"], "gw", "claude-other", "default")
	if entry.reasoningScope != "gateway-shared" {
		t.Fatalf("scope = %q, want the declared token", entry.reasoningScope)
	}
	compiled, err := compileGenerate("claude-other", entry)(
		context.Background(),
		model.ModelRef{
			ID:      model.ModelID{Provider: "gw", Name: "claude-other"},
			Profile: "default",
		},
		reasoningHistoryRequest(message.ReasoningPart{
			Text:      "shared thinking",
			Signature: "sig-shared",
			Source:    "gateway-shared",
		}),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	decision, ok := decisionFor(
		compiled.Report.Decisions, inference.FieldGenerateContextReasoning)
	if !ok || decision.Disposition != inference.Native {
		t.Fatalf("decision = %+v, want Native under the declared scope", decision)
	}
}

// TestReasoningScopeSpecValidation pins the declared token's shape rules.
func TestReasoningScopeSpecValidation(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		wantErr bool
	}{
		{raw: `{}`},
		{raw: `{"wire":{"reasoning_scope":"prod-eu"}}`},
		{raw: `{"wire":{"reasoning_scope":" prod"}}`, wantErr: true},
		{raw: `{"wire":{"reasoning_scope":"prod\neu"}}`, wantErr: true},
		{raw: `{"wire":{"reasoning_scope":"` + strings.Repeat("x", 129) + `"}}`,
			wantErr: true},
	} {
		_, err := decodeSpec(context.Background(), []byte(tc.raw))
		if tc.wantErr && err == nil {
			t.Errorf("decodeSpec(%s) succeeded, want an error", tc.raw)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("decodeSpec(%s): %v", tc.raw, err)
		}
	}
}
