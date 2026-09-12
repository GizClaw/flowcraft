package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

// jsonSetRequest attaches one GenerateOptions extension carrying the escape
// hatch to a plain text request.
func jsonSetRequest(entries map[string]json.RawMessage) inference.GenerateRequest {
	request := simpleTextRequest("hi")
	request.Extensions = inference.Extensions{
		GenerateOptions{Provider: providerID, JSONSet: entries},
	}
	return request
}

// TestJSONSetRidesTheChatBody drives the escape hatch end to end on the chat
// surface: both a top-level field and a nested leaf reach the serialized body
// exactly as written, without disturbing the compiler's own fields.
func TestJSONSetRidesTheChatBody(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		_ *http.Request,
		_ map[string]any,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id": "chatcmpl_1",
			"object": "chat.completion",
			"choices": [{"index": 0,
				"message": {"role": "assistant", "content": "ok"},
				"finish_reason": "stop"}]
		}`)
	})
	defer server.Close()

	entry := catalog["gpt-5.6-sol"]
	entry.dialect.api = apiChat
	request := jsonSetRequest(map[string]json.RawMessage{
		"enable_thinking": json.RawMessage(`false`),
		"thinking.keep":   json.RawMessage(`"all"`),
	})
	compiled, err := compileChat("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileChat: %v", err)
	}
	if _, err := transportChatGenerate(testClients(t, server).api)(
		context.Background(), compiled.Wire,
	); err != nil {
		t.Fatalf("transport: %v", err)
	}

	body := capture.body(0)
	if thinking, ok := body["enable_thinking"].(bool); !ok || thinking {
		t.Fatalf("enable_thinking = %v, want false", body["enable_thinking"])
	}
	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %#v, want an object", body["thinking"])
	}
	if thinking["keep"] != "all" {
		t.Fatalf("thinking.keep = %v, want \"all\"", thinking["keep"])
	}
	// The compiler's own fields are untouched by the patch.
	if body["model"] != "gpt-5.6-sol" {
		t.Fatalf("model = %v", body["model"])
	}
	if _, ok := body["messages"].([]any); !ok {
		t.Fatalf("messages = %#v, want the compiled array", body["messages"])
	}
}

// TestJSONSetRidesTheResponsesBody covers the other JSON surface: a whole
// object at one key, which is how an endpoint that takes a nested dialect
// object is configured.
func TestJSONSetRidesTheResponsesBody(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		_ *http.Request,
		_ map[string]any,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			textOutputItem("ok"),
		}))
	})
	defer server.Close()

	request := jsonSetRequest(map[string]json.RawMessage{
		"thinking": json.RawMessage(`{"type":"enabled","budget_tokens":8192}`),
	})
	compiled, err := compileResponses("gpt-5.6-sol", catalog["gpt-5.6-sol"])(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileResponses: %v", err)
	}
	if _, err := transportGenerate(testClients(t, server).api)(
		context.Background(), compiled.Wire,
	); err != nil {
		t.Fatalf("transport: %v", err)
	}

	thinking, ok := capture.body(0)["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %#v, want an object", capture.body(0)["thinking"])
	}
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(8192) {
		t.Fatalf("thinking = %#v, want the injected object", thinking)
	}
}

// TestJSONSetRejectsReservedRoots pins the boundary: a key under a field the
// compiler lowers (or one that changes the response contract) is a
// configuration error, because the typed knob and the ledger are the only
// honest way to set it.
func TestJSONSetRejectsReservedRoots(t *testing.T) {
	for _, path := range []string{
		"model",
		"messages",
		"messages.0.content",
		"messages[0].content",
		"input",
		"tools",
		"tool_choice",
		"response_format",
		"max_output_tokens",
		"temperature",
		"store",
		"metadata",
		"reasoning.effort",
		"reasoning_effort",
		"prompt_cache_key",
		"n",
	} {
		options := GenerateOptions{JSONSet: map[string]json.RawMessage{
			path: json.RawMessage(`true`),
		}}
		err := options.Validate()
		if err == nil {
			t.Errorf("json_set %q was accepted, want a reserved-field error", path)
			continue
		}
		if !strings.Contains(err.Error(), "canonical request") {
			t.Errorf("json_set %q error = %v, want it to name the rule", path, err)
		}
	}

	for _, path := range []string{
		"enable_thinking",
		"thinking.keep",
		"thinking",
		"extra_body",
		"chat_template_kwargs.enable_thinking",
	} {
		options := GenerateOptions{JSONSet: map[string]json.RawMessage{
			path: json.RawMessage(`true`),
		}}
		if err := options.Validate(); err != nil {
			t.Errorf("json_set %q: %v", path, err)
		}
	}
}

// TestJSONSetBoundsAndShape pins the rest of the validation contract: keys and
// values are bounded, values must be JSON, and the ledger name for a path is a
// valid identifier.
func TestJSONSetBoundsAndShape(t *testing.T) {
	tooMany := make(map[string]json.RawMessage, maxJSONSetKeys+1)
	for index := 0; index <= maxJSONSetKeys; index++ {
		tooMany[fmt.Sprintf("field_%d", index)] = json.RawMessage(`1`)
	}
	if err := (GenerateOptions{JSONSet: tooMany}).Validate(); err == nil {
		t.Error("more keys than the cap were accepted")
	}

	oversized := map[string]json.RawMessage{
		"blob": json.RawMessage(strings.Repeat("x", maxJSONSetBytes+1)),
	}
	if err := (GenerateOptions{JSONSet: oversized}).Validate(); err == nil {
		t.Error("a value over the byte cap was accepted")
	}
	// A raw string is not a JSON value.
	if err := (GenerateOptions{JSONSet: map[string]json.RawMessage{
		"field": json.RawMessage(`not json`),
	}}).Validate(); err == nil {
		t.Error("a non-JSON value was accepted")
	}
	if err := (GenerateOptions{JSONSet: map[string]json.RawMessage{
		"": json.RawMessage(`1`),
	}}).Validate(); err == nil {
		t.Error("an empty key was accepted")
	}
}

// TestJSONSetActiveFields pins the ledger side: one field per distinct
// flattened path, sorted so the report does not inherit map order, and
// always a legal identifier.
func TestJSONSetActiveFields(t *testing.T) {
	options := GenerateOptions{JSONSet: map[string]json.RawMessage{
		"thinking.type":   json.RawMessage(`"enabled"`),
		"thinking.keep":   json.RawMessage(`"all"`),
		"thinking_keep":   json.RawMessage(`"duplicate"`),
		"_gateway_hint":   json.RawMessage(`true`),
		"vendor.Field[0]": json.RawMessage(`1`),
	}}
	got := make([]string, 0, len(options.ActiveFields()))
	for _, field := range options.ActiveFields() {
		got = append(got, string(field))
	}
	want := []string{
		"field_gateway_hint",
		"thinking_keep",
		"thinking_type",
		"vendor_Field_0_",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("active fields = %v, want %v", got, want)
	}
}

// TestJSONSetCloneIsDeep guards the per-attempt copy: the extension travels
// onto every attempt, and one attempt must not hand the caller's backing array
// to the next.
func TestJSONSetCloneIsDeep(t *testing.T) {
	value := json.RawMessage(`{"token":"abc"}`)
	options := GenerateOptions{JSONSet: map[string]json.RawMessage{"field": value}}
	cloned, ok := options.Clone().(GenerateOptions)
	if !ok {
		t.Fatalf("Clone returned %T, want GenerateOptions", options.Clone())
	}
	cloned.JSONSet["field"][2] = 'X'
	if string(options.JSONSet["field"]) != string(value) {
		t.Fatalf("clone shares its value with the original: %s", options.JSONSet["field"])
	}
}

// TestJSONSetIsLedgerNative pins the reporting contract: a carried key is a
// Native decision the report names, so the ledger never claims a setting the
// request did not carry.
func TestJSONSetIsLedgerNative(t *testing.T) {
	entry := catalog["gpt-5.6-sol"]
	entry.dialect.api = apiChat
	compiled, err := compileChat("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		jsonSetRequest(map[string]json.RawMessage{
			"enable_thinking": json.RawMessage(`false`),
		}),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileChat: %v", err)
	}
	field := inference.FieldID(
		"extension." + providerID + ".generate_options.enable_thinking",
	)
	decision, ok := decisionFor(compiled.Report.Decisions, field)
	if !ok {
		t.Fatalf("compiled report has no decision for %q", field)
	}
	if decision.Disposition != inference.Native {
		t.Fatalf("decision = %+v, want Native", decision)
	}
}

// TestJSONSetCannotWriteTheMetadataEnvelope pins the one collision the
// request-level validation cannot see: a deployment that forwards request
// metadata owns its envelope field, so json_set writing that name is rejected
// rather than silently racing the metadata patch.
func TestJSONSetCannotWriteTheMetadataEnvelope(t *testing.T) {
	entry := catalog["gpt-5.6-sol"]
	entry.dialect.api = apiChat
	entry.dialect.requestMetadataEnvelope = "request_fields"
	request := jsonSetRequest(map[string]json.RawMessage{
		"request_fields":  json.RawMessage(`{"conversation":"42"}`),
		"enable_thinking": json.RawMessage(`false`),
	})
	compiled, err := compileChat("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatalf("compileChat succeeded, want the envelope collision rejected")
	}
	field := inference.FieldID(
		"extension." + providerID + ".generate_options.request_fields",
	)
	decision, ok := decisionFor(compiled.Report.Decisions, field)
	if !ok || decision.Disposition != inference.Rejected {
		t.Fatalf("decision for %q = %+v, want Rejected", field, decision)
	}
	if !strings.Contains(decision.Reason, "request_metadata envelope") {
		t.Fatalf("reason = %q", decision.Reason)
	}
	// The other key still rode the request: one rejected key does not discard
	// the rest of the escape hatch.
	if compiled.Report.Rejects(
		inference.FieldID("extension." + providerID + ".generate_options.enable_thinking"),
	) {
		t.Fatalf("enable_thinking was rejected: %+v", compiled.Report.Decisions)
	}
}

// TestExtraBodyRidesEveryRequest pins the deployment-level twin: a body field
// declared in spec.wire.extra_body reaches every request the deployment
// serves, on both JSON surfaces, without becoming a request decision (the
// compile report has no entry for it).
func TestExtraBodyRidesEveryRequest(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		r *http.Request,
		_ map[string]any,
	) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/chat/completions" {
			_, _ = fmt.Fprint(w, chatResponseJSON)
			return
		}
		_, _ = fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			textOutputItem("ok"),
		}))
	})
	defer server.Close()

	entryWith := func(t *testing.T, raw string) catalogEntry {
		t.Helper()
		spec, err := decodeSpec(context.Background(), []byte(raw))
		if err != nil {
			t.Fatalf("decodeSpec: %v", err)
		}
		entry := catalog["gpt-5.6-sol"]
		entry.dialect = spec.dialect()
		return entry
	}
	assertField := func(t *testing.T, report inference.CompileReport) {
		t.Helper()
		thinking, ok := capture.body(0)["thinking"].(map[string]any)
		if !ok || thinking["type"] != "enabled" {
			t.Fatalf("thinking = %#v, want the deployment field",
				capture.body(0)["thinking"])
		}
		// Deployment configuration is not a request decision: no ledger
		// entry, and the successful report stays valid without one.
		if _, ok := decisionFor(
			report.Decisions,
			inference.FieldID("extension."+providerID+".generate_options.thinking"),
		); ok {
			t.Fatalf("extra_body produced a request decision: %+v", report.Decisions)
		}
	}

	t.Run("chat", func(t *testing.T) {
		entry := entryWith(t,
			`{"api":"chat","wire":{"extra_body":{"thinking":{"type":"enabled"}}}}`)
		compiled, err := compileChat("gpt-5.6-sol", entry)(
			context.Background(),
			openaiModel("gpt-5.6-sol"),
			simpleTextRequest("hi"),
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			t.Fatalf("compileChat: %v", err)
		}
		if _, err := transportChatGenerate(testClients(t, server).api)(
			context.Background(), compiled.Wire,
		); err != nil {
			t.Fatalf("transport: %v", err)
		}
		assertField(t, compiled.Report)
	})

	t.Run("responses", func(t *testing.T) {
		entry := entryWith(t,
			`{"wire":{"extra_body":{"thinking":{"type":"enabled"}}}}`)
		compiled, err := compileResponses("gpt-5.6-sol", entry)(
			context.Background(),
			openaiModel("gpt-5.6-sol"),
			simpleTextRequest("hi"),
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			t.Fatalf("compileResponses: %v", err)
		}
		if _, err := transportGenerate(testClients(t, server).api)(
			context.Background(), compiled.Wire,
		); err != nil {
			t.Fatalf("transport: %v", err)
		}
		assertField(t, compiled.Report)
	})
}

// TestJSONSetWinsOverExtraBody pins the precedence rule between the two
// sources: the deployment default is written first, so a request key merges
// into it (a nested leaf) or replaces it (an identical path).
func TestJSONSetWinsOverExtraBody(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		_ *http.Request,
		_ map[string]any,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, chatResponseJSON)
	})
	defer server.Close()

	spec, err := decodeSpec(context.Background(), []byte(
		`{"api":"chat","wire":{"extra_body":`+
			`{"thinking":{"type":"enabled","keep":"all"},"enable_thinking":false}}}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	entry := catalog["gpt-5.6-sol"]
	entry.dialect = spec.dialect()

	request := jsonSetRequest(map[string]json.RawMessage{
		"thinking.keep":   json.RawMessage(`"none"`),
		"enable_thinking": json.RawMessage(`true`),
	})
	compiled, err := compileChat("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileChat: %v", err)
	}
	if _, err := transportChatGenerate(testClients(t, server).api)(
		context.Background(), compiled.Wire,
	); err != nil {
		t.Fatalf("transport: %v", err)
	}

	body := capture.body(0)
	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %#v", body["thinking"])
	}
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking.type = %v, want the deployment value kept", thinking["type"])
	}
	if thinking["keep"] != "none" {
		t.Fatalf("thinking.keep = %v, want the request value", thinking["keep"])
	}
	if enabled, ok := body["enable_thinking"].(bool); !ok || !enabled {
		t.Fatalf("enable_thinking = %v, want the request value", body["enable_thinking"])
	}
}

// TestExtraBodyValidation pins the deployment-level validation: the same
// bounds and reserved keys as json_set, plus the metadata envelope collision
// that this surface can see at build time.
func TestExtraBodyValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "plain field", raw: `{"wire":{"extra_body":{"enable_thinking":false}}}`},
		{name: "nested path", raw: `{"wire":{"extra_body":{"thinking.keep":"all"}}}`},
		{name: "generate model declared",
			raw: `{"catalog":"declared","wire":{"extra_body":{"enable_thinking":false}},` +
				`"models":[{"name":"m","kind":"generate"}]}`},
		{name: "no generate surface",
			raw: `{"catalog":"declared","wire":{"extra_body":{"enable_thinking":false}},` +
				`"models":[{"name":"m","kind":"image"}]}`, wantErr: true},
		{name: "reserved key",
			raw: `{"wire":{"extra_body":{"messages":[]}}}`, wantErr: true},
		{name: "reserved nested key",
			raw: `{"wire":{"extra_body":{"reasoning.effort":"low"}}}`, wantErr: true},
		{name: "empty key",
			raw: `{"wire":{"extra_body":{"":"x"}}}`, wantErr: true},
		{name: "envelope collision",
			raw: `{"wire":{"extra_body":{"request_fields":{}}},` +
				`"request_metadata":{"envelope":"request_fields"}}`, wantErr: true},
		{name: "bytes over the cap",
			raw: `{"wire":{"extra_body":{"blob":"` +
				strings.Repeat("x", maxJSONSetBytes+1) + `"}}}`, wantErr: true},
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
