package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// reasoningContextRequest is a request whose assistant history carries one
// reasoning trace.
func reasoningContextRequest(part message.ReasoningPart) inference.GenerateRequest {
	request := simpleTextRequest("current")
	request.Context = []message.Message{{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			part,
			message.TextPart{Text: "answer"},
		}},
	}}
	return request
}

// reasoningDecision returns the context-reasoning decision of one compile.
func reasoningDecision(t *testing.T, report inference.CompileReport) inference.Decision {
	t.Helper()
	decision, ok := decisionFor(
		report.Decisions, inference.FieldGenerateContextReasoning)
	if !ok {
		t.Fatalf("no decision for context reasoning: %+v", report.Decisions)
	}
	return decision
}

// TestReasoningFromAnotherScopeIsDroppedNotSent covers the failure this rule
// exists for: a trace another model, account, or deployment produced passes
// the shape check (it has an item id and an encrypted payload) but the target
// cannot verify it, so sending it is a provider error no retry can repair. It
// is dropped instead.
func TestReasoningFromAnotherScopeIsDroppedNotSent(t *testing.T) {
	entry := scopedEntry(catalog["gpt-5.6-sol"], "gpt-5.6-sol")
	compiled, err := compileResponses("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		reasoningContextRequest(message.ReasoningPart{
			Text:      "their thinking",
			Signature: "enc-foreign",
			ID:        "rs_foreign",
			Source:    "openai/gpt-5.6-terra/default",
		}),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileResponses: %v", err)
	}
	decision := reasoningDecision(t, compiled.Report)
	if decision.Disposition != inference.Dropped {
		t.Fatalf("decision = %+v, want Dropped", decision)
	}
	if !strings.Contains(decision.Reason, "openai/gpt-5.6-terra/default") ||
		!strings.Contains(decision.Reason, "openai/gpt-5.6-sol/default") {
		t.Fatalf("reason = %q, want both scopes named", decision.Reason)
	}
	assertNoReasoningItem(t, compiled)
}

// TestReasoningWithoutProvenanceIsDropped covers a transcript stored before
// drivers stamped their traces: unattributable reasoning is not replayed,
// because nothing says the target can verify it.
func TestReasoningWithoutProvenanceIsDropped(t *testing.T) {
	entry := scopedEntry(catalog["gpt-5.6-sol"], "gpt-5.6-sol")
	compiled, err := compileResponses("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		reasoningContextRequest(message.ReasoningPart{
			Text:      "legacy thinking",
			Signature: "enc-legacy",
			ID:        "rs_legacy",
		}),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileResponses: %v", err)
	}
	decision := reasoningDecision(t, compiled.Report)
	if decision.Disposition != inference.Dropped ||
		!strings.Contains(decision.Reason, "no provenance") {
		t.Fatalf("decision = %+v, want a provenance drop", decision)
	}
	assertNoReasoningItem(t, compiled)
}

// TestDeclaredReasoningScopeSharesTraces pins the escape hatch: a deployment
// that declares a scope claims its models and credentials verify each other's
// traces, so a trace stamped with that token replays even though its derived
// address would differ.
func TestDeclaredReasoningScopeSharesTraces(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"wire":{"reasoning_scope":"openai-prod-shared"}}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	entry := catalog["gpt-5.6-sol"]
	entry.dialect = spec.dialect()
	entry.reasoningScope = inference.ReasoningScope(
		entry.dialect.reasoningScopeDeclared,
		"openai", "gpt-5.6-sol", "default",
	)
	if entry.reasoningScope != "openai-prod-shared" {
		t.Fatalf("scope = %q, want the declared token", entry.reasoningScope)
	}

	compiled, err := compileResponses("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		reasoningContextRequest(message.ReasoningPart{
			Text:      "shared thinking",
			Signature: "enc-shared",
			ID:        "rs_shared",
			Source:    "openai-prod-shared",
		}),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileResponses: %v", err)
	}
	decision := reasoningDecision(t, compiled.Report)
	if decision.Disposition != inference.Native {
		t.Fatalf("decision = %+v, want Native", decision)
	}
	items := compiled.Wire.params.Input.OfInputItemList
	if len(items) == 0 || items[0].OfReasoning == nil {
		t.Fatalf("items = %+v, want the reasoning item first", items)
	}
}

// TestReasoningModelSwitchDropsTraces pins the derived default's consequence:
// switching models inside one deployment changes the scope, so the previous
// model's traces are dropped rather than replayed — which is what the
// providers that bind a trace to a model require, and what we cannot verify
// for the ones that do not document it.
func TestReasoningModelSwitchDropsTraces(t *testing.T) {
	trace := message.ReasoningPart{
		Text:      "luna thinking",
		Signature: "enc-luna",
		ID:        "rs_luna",
		Source:    reasoningScopeFor("gpt-5.6-luna"),
	}
	entry := scopedEntry(catalog["gpt-5.6-terra"], "gpt-5.6-terra")
	compiled, err := compileResponses("gpt-5.6-terra", entry)(
		context.Background(),
		openaiModel("gpt-5.6-terra"),
		reasoningContextRequest(trace),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileResponses: %v", err)
	}
	if decision := reasoningDecision(t, compiled.Report); decision.Disposition != inference.Dropped {
		t.Fatalf("decision = %+v, want Dropped across models", decision)
	}
	assertNoReasoningItem(t, compiled)
}

// TestReasoningScopeSpecValidation pins the declared token's shape rules.
func TestReasoningScopeSpecValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "unset", raw: `{}`},
		{name: "token", raw: `{"wire":{"reasoning_scope":"prod-eu"}}`},
		{name: "token with a separator",
			raw: `{"wire":{"reasoning_scope":"openai/gpt-5.6-sol"}}`},
		{name: "surrounding whitespace",
			raw: `{"wire":{"reasoning_scope":" prod"}}`, wantErr: true},
		{name: "control character",
			raw: `{"wire":{"reasoning_scope":"prod\neu"}}`, wantErr: true},
		{name: "over the cap",
			raw:     `{"wire":{"reasoning_scope":"` + strings.Repeat("x", 129) + `"}}`,
			wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeSpec(context.Background(), []byte(tc.raw))
			if tc.wantErr && err == nil {
				t.Fatal("decodeSpec succeeded, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("decodeSpec: %v", err)
			}
		})
	}
}

// assertNoReasoningItem checks that no reasoning item reached the request.
func assertNoReasoningItem(t *testing.T, compiled inference.Compiled[*responsesRequest]) {
	t.Helper()
	for _, item := range compiled.Wire.params.Input.OfInputItemList {
		if item.OfReasoning != nil {
			t.Fatalf("dropped reasoning reached the request: %+v", item)
		}
	}
}
