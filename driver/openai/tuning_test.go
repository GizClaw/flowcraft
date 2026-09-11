package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

// TestReasoningSummaryReachesTheWire pins the opt-in that makes reasoning
// traces readable: without it the API returns the encrypted payload and no
// summary text at all.
func TestReasoningSummaryReachesTheWire(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"wire":{"reasoning_summary":"detailed"}}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["gpt-5.6-sol"]
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text = &inference.TextIntent{
		ReasoningEffort: inference.ReasoningHigh,
	}
	compiled, err := compileGenerate("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	params := wireToParams(compiled.Wire)
	if string(params.Reasoning.Summary) != "detailed" ||
		string(params.Reasoning.Effort) != "high" {
		t.Fatalf("reasoning = %+v", params.Reasoning)
	}

	// A summary alone (no explicit effort) must still reach the request.
	compiled, err = compileGenerate("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		simpleTextRequest("hi"),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	params = wireToParams(compiled.Wire)
	if string(params.Reasoning.Summary) != "detailed" {
		t.Fatalf("reasoning = %+v", params.Reasoning)
	}
}

// TestPromptCacheKeyRidesTheExtension: the cache key is OpenAI-wire
// vocabulary, so it travels as a per-call extension on both surfaces rather
// than as a canonical request field every provider would have to answer for.
func TestPromptCacheKeyRidesTheExtension(t *testing.T) {
	for _, api := range []string{"responses", "chat"} {
		entry := catalog["gpt-5.6-sol"]
		if api == "chat" {
			entry.api = apiChat
		}
		wire := compileTextWire(t, simpleTextRequest("hi"))
		ledger := newLedger(inference.OperationGenerate, nil)
		compileGenerateTuning(&wire, GenerateOptions{
			PromptCacheKey: "conversation-42",
		}, entry, ledger)
		if wire.promptCacheKey != "conversation-42" {
			t.Fatalf("%s wire = %+v", api, wire.promptCacheKey)
		}
		params := wireToParams(wire)
		if params.PromptCacheKey.Value != "conversation-42" {
			t.Fatalf("%s params = %+v", api, params.PromptCacheKey)
		}
	}

	chat := compileTextWire(t, simpleTextRequest("hi"))
	chatParams := wireToChatParams(chat)
	if chatParams.PromptCacheKey.Value != "" {
		t.Fatalf("unset key leaked: %+v", chatParams.PromptCacheKey)
	}
}

// TestGenerateTuningExtensions covers the per-call knobs: they land where the
// surface carries them and are rejected with a qualified field where it does
// not.
func TestGenerateTuningExtensions(t *testing.T) {
	calls := 4
	parallel := false
	options := GenerateOptions{
		ServiceTier:       "priority",
		ParallelToolCalls: &parallel,
		MaxToolCalls:      &calls,
		Verbosity:         "low",
		SafetyIdentifier:  "hashed-user",
	}
	if err := options.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	wire := compileTextWire(t, simpleTextRequest("hi"))
	ledger := newLedger(inference.OperationGenerate, nil)
	compileGenerateTuning(&wire, options, catalog["gpt-5.6-sol"], ledger)
	if wire.serviceTier != "priority" ||
		wire.parallelToolCalls == nil || *wire.parallelToolCalls ||
		wire.maxToolCalls == nil || *wire.maxToolCalls != 4 ||
		wire.verbosity != "low" ||
		wire.safetyIdentifier != "hashed-user" {
		t.Fatalf("wire = %+v", wire)
	}

	params := wireToParams(wire)
	if string(params.ServiceTier) != "priority" ||
		params.ParallelToolCalls.Value ||
		params.MaxToolCalls.Value != 4 ||
		params.SafetyIdentifier.Value != "hashed-user" ||
		string(params.Text.Verbosity) != "low" {
		t.Fatalf("params = %+v", params)
	}
}

// TestGenerateTuningRejectsChatOnlyGaps: Chat Completions has no verbosity or
// tool-call budget, so those must fail loudly rather than disappear.
func TestGenerateTuningRejectsChatOnlyGaps(t *testing.T) {
	calls := 2
	wire := generateWire{}
	options := GenerateOptions{MaxToolCalls: &calls, Verbosity: "high"}
	active := make([]inference.FieldID, 0, len(options.ActiveFields()))
	for _, field := range options.ActiveFields() {
		active = append(active, field.Qualify(options))
	}
	ledger := newLedger(inference.OperationGenerate, active)
	entry := catalog["gpt-5.6-sol"]
	entry.api = apiChat
	compileGenerateTuning(&wire, options, entry, ledger)
	if wire.maxToolCalls != nil || wire.verbosity != "" {
		t.Fatalf("chat wire = %+v", wire)
	}
	report := ledger.report()
	for _, want := range []inference.FieldID{
		inference.ExtensionField("max_tool_calls").Qualify(options),
		inference.ExtensionField("verbosity").Qualify(options),
	} {
		found := false
		for _, decision := range report.Decisions {
			if decision.Field == want && decision.Disposition == inference.Rejected {
				found = true
			}
		}
		if !found {
			t.Fatalf("no rejection for %s in %+v", want, report.Decisions)
		}
	}
}

// TestTruncationAndTimeout pin the two deployment-level additions: the
// overflow policy rides the raw-JSON option path, and the timeout reaches the
// HTTP client.
func TestTruncationAndTimeout(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"endpoint":{"timeout":"90s"},"wire":{"truncation":"auto"}}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if spec.endpointTimeout().Seconds() != 90 {
		t.Fatalf("timeout = %v", spec.endpointTimeout())
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	wire := compileTextWire(t, simpleTextRequest("hi"))
	wire.truncation = models["gpt-5.6-sol"].truncation
	if wire.truncation != truncationAuto {
		t.Fatalf("truncation = %q", wire.truncation)
	}
	if opts := requestOverflowOptions(wire); len(opts) != 1 {
		t.Fatalf("overflow options = %v", opts)
	}
}

// TestAuthNoneDropsCredentials: a local gateway that authenticates nothing
// must not receive a bearer header invented by the SDK.
func TestAuthNoneDropsCredentials(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			textOutputItem("ok"),
		}))
	}))
	defer server.Close()

	spec, err := decodeSpec(context.Background(), []byte(fmt.Sprintf(
		`{"endpoint":{"base_url":%q},"auth":{"scheme":"none"}}`, server.URL,
	)))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	cls, err := profileMaterial{}.newClients(context.Background(), spec)
	if err != nil {
		t.Fatalf("newClients: %v", err)
	}
	if _, err := transportGenerate(cls.api)(
		context.Background(),
		compileTextWire(t, simpleTextRequest("hi")),
	); err != nil {
		t.Fatalf("transportGenerate: %v", err)
	}
	if authorization != "" {
		t.Fatalf("authorization = %q, want none", authorization)
	}
}
