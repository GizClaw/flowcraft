package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"

	"github.com/openai/openai-go/v3/responses"
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
		ReasoningEffort: model.ReasoningHigh,
	}
	compiled, err := compileResponses("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileResponses: %v", err)
	}
	params := compiled.Wire.params
	if string(params.Reasoning.Summary) != "detailed" ||
		string(params.Reasoning.Effort) != "high" {
		t.Fatalf("reasoning = %+v", params.Reasoning)
	}

	// A summary alone (no explicit effort) must still reach the request.
	compiled, err = compileResponses("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		simpleTextRequest("hi"),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileResponses: %v", err)
	}
	params = compiled.Wire.params
	if string(params.Reasoning.Summary) != "detailed" {
		t.Fatalf("reasoning = %+v", params.Reasoning)
	}
}

// TestPromptCacheKeyRidesTheExtension: the cache key is OpenAI-wire
// vocabulary, so it travels as a per-call extension on both surfaces rather
// than as a canonical request field every provider would have to answer for.
func TestPromptCacheKeyRidesTheExtension(t *testing.T) {
	options := GenerateOptions{PromptCacheKey: "conversation-42"}

	responsesEntry := catalog["gpt-5.6-sol"]
	responses := newResponsesRequest("gpt-5.6-sol", responsesEntry)
	compileGenerateTuning(responses, options, responsesEntry,
		inference.NewLedger(model.OperationGenerate, providerID, nil))
	if responses.params.PromptCacheKey.Value != "conversation-42" {
		t.Fatalf("responses params = %+v", responses.params.PromptCacheKey)
	}

	chatEntry := catalog["gpt-5.6-sol"]
	chatEntry.dialect.api = apiChat
	chat := newChatRequest("gpt-5.6-sol", chatEntry, inference.GenerateExecutionUnary)
	compileGenerateTuning(chat, options, chatEntry,
		inference.NewLedger(model.OperationGenerate, providerID, nil))
	if chat.params.PromptCacheKey.Value != "conversation-42" {
		t.Fatalf("chat params = %+v", chat.params.PromptCacheKey)
	}

	unset := newChatRequest("gpt-5.6-sol", chatEntry, inference.GenerateExecutionUnary)
	if unset.params.PromptCacheKey.Value != "" {
		t.Fatalf("unset key leaked: %+v", unset.params.PromptCacheKey)
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
	entry := catalog["gpt-5.6-sol"]
	request := newResponsesRequest("gpt-5.6-sol", entry)
	ledger := inference.NewLedger(model.OperationGenerate, providerID, nil)
	compileGenerateTuning(request, options, entry, ledger)
	params := request.params
	if string(params.ServiceTier) != "priority" ||
		params.ParallelToolCalls.Value ||
		params.MaxToolCalls.Value != 4 ||
		params.SafetyIdentifier.Value != "hashed-user" ||
		string(params.Text.Verbosity) != "low" {
		t.Fatalf("params = %+v", params)
	}
}

// TestGenerateTuningChatGaps: Chat Completions has no tool-call budget, so
// that knob must fail loudly rather than disappear; verbosity is part of the
// chat surface and reaches the request.
func TestGenerateTuningChatGaps(t *testing.T) {
	calls := 2
	options := GenerateOptions{MaxToolCalls: &calls, Verbosity: "high"}
	active := make([]inference.FieldID, 0, len(options.ActiveFields()))
	for _, field := range options.ActiveFields() {
		active = append(active, field.Qualify(options))
	}
	entry := catalog["gpt-5.6-sol"]
	entry.dialect.api = apiChat
	ledger := inference.NewLedger(model.OperationGenerate, providerID, active)
	request := newChatRequest("gpt-5.6-sol", entry, inference.GenerateExecutionUnary)
	compileGenerateTuning(request, options, entry, ledger)
	if string(request.params.Verbosity) != "high" {
		t.Fatalf("chat verbosity = %q, want high", request.params.Verbosity)
	}
	report := ledger.Report()
	for _, want := range []inference.FieldID{
		inference.ExtensionField("max_tool_calls").Qualify(options),
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
	if report.Rejects(inference.ExtensionField("verbosity").Qualify(options)) {
		t.Fatalf("chat verbosity must not be rejected: %+v", report.Decisions)
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
	entry := models["gpt-5.6-sol"]
	if entry.dialect.truncation != truncationAuto {
		t.Fatalf("truncation = %q", entry.dialect.truncation)
	}
	request := newResponsesRequest("gpt-5.6-sol", entry)
	if request.params.Truncation != responses.ResponseNewParamsTruncationAuto {
		t.Fatalf("truncation param = %q", request.params.Truncation)
	}
	// The SDK types the field now, so the policy rides the body instead of
	// the raw-JSON option path.
	body, err := json.Marshal(request.params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	if !strings.Contains(string(body), `"truncation":"auto"`) {
		t.Fatalf("body = %s, want truncation on the request", body)
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
		compileTextRequest(t, simpleTextRequest("hi")),
	); err != nil {
		t.Fatalf("transportGenerate: %v", err)
	}
	if authorization != "" {
		t.Fatalf("authorization = %q, want none", authorization)
	}
}
