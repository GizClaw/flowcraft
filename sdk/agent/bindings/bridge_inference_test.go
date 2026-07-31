package bindings

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
	"github.com/GizClaw/flowcraft/sdk/inference/route"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// The inference bridge projects wire JSON straight into Runtime/Router
// calls, so the tests run real Runtime and Router instances over the
// canned provider in inferencetest — no provider I/O, but the full
// resolve/compile/validate pipeline is exercised.

func fakeRouter(t *testing.T, runtime *inference.Runtime) *route.Router {
	t.Helper()
	router, err := route.New(runtime, route.Selectors{
		Generate: inferencetest.StaticGenerateSelector(inferencetest.DefaultFakeModel),
	})
	if err != nil {
		t.Fatalf("route.New: %v", err)
	}
	return router
}

type inferenceAPI struct {
	generate    func(raw any) (any, error)
	route       func(raw any) (any, error)
	stream      func(raw any) (any, error)
	routeStream func(raw any) (any, error)
}

func newInferenceAPI(t *testing.T, runtime *inference.Runtime, router *route.Router, opts ...InferenceBridgeOption) inferenceAPI {
	t.Helper()
	name, raw := NewInferenceBridge(runtime, router, opts...)(context.Background())
	if name != "inference" {
		t.Fatalf("binding name = %q, want %q", name, "inference")
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("binding value = %T, want map[string]any", raw)
	}
	generate, ok := m["generate"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.generate = %T", m["generate"])
	}
	routeFn, ok := m["route"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.route = %T", m["route"])
	}
	streamFn, ok := m["stream"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.stream = %T", m["stream"])
	}
	routeStreamFn, ok := m["routeStream"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.routeStream = %T", m["routeStream"])
	}
	return inferenceAPI{generate: generate, route: routeFn, stream: streamFn, routeStream: routeStreamFn}
}

// streamHandle mirrors the script-facing iterator triple.
type streamHandle struct {
	next   func() (any, error)
	result func() (any, error)
	close  func() error
}

func openStream(t *testing.T, raw any) streamHandle {
	t.Helper()
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("stream handle = %T, want map[string]any", raw)
	}
	next, ok := m["next"].(func() (any, error))
	if !ok {
		t.Fatalf("stream.next = %T", m["next"])
	}
	result, ok := m["result"].(func() (any, error))
	if !ok {
		t.Fatalf("stream.result = %T", m["result"])
	}
	closeFn, ok := m["close"].(func() error)
	if !ok {
		t.Fatalf("stream.close = %T", m["close"])
	}
	return streamHandle{next: next, result: result, close: closeFn}
}

func fakeModelJSON() map[string]any {
	return map[string]any{
		"id":      map[string]any{"provider": "fake", "name": "echo"},
		"profile": "default",
	}
}

func userInput(text string) map[string]any {
	return map[string]any{
		"role": "user",
		"content": map[string]any{
			"parts":  []any{map[string]any{"type": "text", "text": text}},
			"intent": map[string]any{"text": map[string]any{}},
		},
	}
}

func TestInferenceBridge_Generate_RoundTrip(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Runtime(t), nil)

	out, err := api.generate(map[string]any{
		"model":   fakeModelJSON(),
		"context": []any{map[string]any{"role": "user", "content": map[string]any{"parts": []any{map[string]any{"type": "text", "text": "earlier"}}}}},
		"input":   userInput("hi"),
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	resp, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("response = %T, want object", out)
	}
	if resp["finish_reason"] != string(inference.FinishCompleted) {
		t.Fatalf("finish_reason = %v, want %q", resp["finish_reason"], inference.FinishCompleted)
	}
	message, ok := resp["message"].(map[string]any)
	if !ok {
		t.Fatalf("message = %T, want object", resp["message"])
	}
	parts, ok := message["content"].(map[string]any)["parts"].([]any)
	if !ok || len(parts) != 1 || parts[0].(map[string]any)["text"] != "ok" {
		t.Fatalf("message parts = %v, want one text part %q", message["content"], "ok")
	}

	// The canonical request reached the provider intact: context plus
	// the current-turn input, in order.
	reqs := fake.Requests()
	if len(reqs) != 1 || len(reqs[0].Context) != 1 || len(reqs[0].Context[0].Content.Parts) != 1 {
		t.Fatalf("provider saw context = %+v, want one message", reqs)
	}
	text, ok := reqs[0].Input.Content.Parts[0].(inference.TextPart)
	if !ok || text.Text != "hi" || reqs[0].Input.Role != inference.InputRoleUser {
		t.Fatalf("provider saw input = %+v, want user text %q", reqs[0].Input, "hi")
	}
}

func TestInferenceBridge_Generate_MissingModel(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Runtime(t), nil)

	_, err := api.generate(map[string]any{"input": userInput("hi")})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("missing model error = %v, want validation-classified", err)
	}
}

func TestInferenceBridge_Generate_StrictUnknownField(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Runtime(t), nil)

	_, err := api.generate(map[string]any{
		"model":  fakeModelJSON(),
		"contex": []any{},
		"input":  userInput("hi"),
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("typo field error = %v, want validation-classified", err)
	}
}

func TestInferenceBridge_Generate_NoRuntime(t *testing.T) {
	api := newInferenceAPI(t, nil, nil)
	_, err := api.generate(map[string]any{
		"model": fakeModelJSON(),
		"input": userInput("hi"),
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired generate error = %v, want NotAvailable", err)
	}
}

func TestInferenceBridge_Generate_UnknownModel(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Runtime(t), nil)

	_, err := api.generate(map[string]any{
		"model": map[string]any{"id": map[string]any{"provider": "fake", "name": "ghost"}, "profile": "default"},
		"input": userInput("hi"),
	})
	if err == nil {
		t.Fatal("unknown model should surface the runtime's resolution error")
	}
}

func TestInferenceBridge_Route_RoundTrip(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	runtime := fake.Runtime(t)
	api := newInferenceAPI(t, runtime, fakeRouter(t, runtime))

	out, err := api.route(map[string]any{"input": userInput("hi")})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	resp, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("response = %T, want object", out)
	}
	if resp["finish_reason"] != string(inference.FinishCompleted) {
		t.Fatalf("finish_reason = %v", resp["finish_reason"])
	}
	trace, ok := resp["trace"].(map[string]any)
	if !ok {
		t.Fatalf("trace missing or %T, want object", resp["trace"])
	}
	executed, ok := trace["executed"].(map[string]any)
	if !ok {
		t.Fatalf("trace.executed = %T, want object", trace["executed"])
	}
	id, ok := executed["id"].(map[string]any)
	if !ok || id["provider"] != "fake" || id["name"] != "echo" {
		t.Fatalf("trace.executed.id = %v, want fake/echo", executed["id"])
	}
	if req := fake.LastRequest(); len(req.Context) != 0 || req.Input.Role != inference.InputRoleUser {
		t.Fatalf("router forwarded request = %+v", req)
	}
}

func TestInferenceBridge_Route_RejectsModelKey(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	runtime := fake.Runtime(t)
	api := newInferenceAPI(t, runtime, fakeRouter(t, runtime))

	// The router owns target selection; a model key is a strict-decode
	// error, not a silent override.
	_, err := api.route(map[string]any{
		"model": fakeModelJSON(),
		"input": userInput("hi"),
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("route with model key error = %v, want validation-classified", err)
	}
}

func TestInferenceBridge_Route_NoRouter(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Runtime(t), nil)

	_, err := api.route(map[string]any{"input": userInput("hi")})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired route error = %v, want NotAvailable", err)
	}
}

func TestInferenceBridge_Generate_ToolCallResponse(t *testing.T) {
	// The multi-turn contract: a tool_calls finish carries the
	// assistant message verbatim, tool_call id included, so the script
	// can forward it to tools.callAll and continue with role="tool".
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: inference.Message{
					Role: inference.RoleAssistant,
					Content: inference.Content{Parts: []inference.Part{
						inference.ToolCallPart{Call: tool.Call{
							ID:        "call_1",
							Name:      "search",
							Arguments: json.RawMessage(`{"q":"weather"}`),
						}},
					}},
				},
				FinishReason: inference.FinishToolCalls,
			}
		},
	}
	api := newInferenceAPI(t, fake.Runtime(t), nil)

	out, err := api.generate(map[string]any{
		"model": fakeModelJSON(),
		"input": map[string]any{
			"role": "user",
			"content": map[string]any{
				"parts": []any{map[string]any{"type": "text", "text": "search please"}},
				// The response contract requires tool calls to name a
				// tool the request declared in the text intent.
				"intent": map[string]any{"text": map[string]any{
					"tools": []any{map[string]any{
						"name":         "search",
						"description":  "search the web",
						"input_schema": map[string]any{"type": "object"},
					}},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	resp := out.(map[string]any)
	if resp["finish_reason"] != string(inference.FinishToolCalls) {
		t.Fatalf("finish_reason = %v, want %q", resp["finish_reason"], inference.FinishToolCalls)
	}
	parts := resp["message"].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	part, ok := parts[0].(map[string]any)
	if !ok || part["type"] != "tool_call" {
		t.Fatalf("part = %v, want a tool_call part", parts[0])
	}
	call, ok := part["call"].(map[string]any)
	if !ok || call["id"] != "call_1" || call["name"] != "search" {
		t.Fatalf("tool_call projection = %v, want call.id call_1 call.name search", part)
	}
}

// testExtension mirrors the provider option-struct pattern (kimi's
// GenerateOptions): JSON-tagged knobs, json:"-" provider override,
// identity + ledger methods.
type testExtension struct {
	Provider string `json:"-"`
	CacheKey string `json:"cache_key,omitempty"`
}

func (e testExtension) ProviderID() string {
	if e.Provider != "" {
		return e.Provider
	}
	return "fake"
}

func (e testExtension) ExtensionID() string { return "generate_options" }

func (e testExtension) ActiveFields() []inference.ExtensionField {
	if e.CacheKey == "" {
		return nil
	}
	return []inference.ExtensionField{"cache_key"}
}

func (e testExtension) Validate() error { return nil }

func (e testExtension) Clone() inference.Extension { return e }

func testExtensionDecoder() ExtensionDecoder {
	return ExtensionDecoderFor(func() *testExtension { return &testExtension{} })
}

func TestInferenceBridge_Generate_Extensions(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Runtime(t), nil,
		WithExtensionDecoder("fake", "generate_options", testExtensionDecoder()))

	_, err := api.generate(map[string]any{
		"model": fakeModelJSON(),
		"input": userInput("hi"),
		"extensions": []any{map[string]any{
			"provider": "fake",
			"id":       "generate_options",
			"fields":   map[string]any{"cache_key": "sess-1"},
		}},
	})
	if err != nil {
		t.Fatalf("generate with extensions: %v", err)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 || len(reqs[0].Extensions) != 1 {
		t.Fatalf("provider saw %+v, want one extension", reqs)
	}
	// The pipeline Clone()s requests in flight, so extensions arrive
	// in the shape their Clone returns — the value form, matching how
	// provider compilers type-assert them.
	ext, ok := reqs[0].Extensions[0].(testExtension)
	if !ok {
		t.Fatalf("extension = %T, want testExtension (post-Clone value form)", reqs[0].Extensions[0])
	}
	if ext.CacheKey != "sess-1" || ext.ProviderID() != "fake" || ext.ExtensionID() != "generate_options" {
		t.Fatalf("extension = %+v, want cache_key sess-1 addressed to fake/generate_options", ext)
	}
}

func TestInferenceBridge_Extensions_Unregistered(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Runtime(t), nil,
		WithExtensionDecoder("fake", "generate_options", testExtensionDecoder()))

	_, err := api.generate(map[string]any{
		"model": fakeModelJSON(),
		"input": userInput("hi"),
		"extensions": []any{map[string]any{
			"provider": "fake",
			"id":       "ghost",
			"fields":   map[string]any{},
		}},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("unregistered extension error = %v, want validation-classified", err)
	}
}

func TestInferenceBridge_Extensions_StrictFields(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Runtime(t), nil,
		WithExtensionDecoder("fake", "generate_options", testExtensionDecoder()))

	_, err := api.generate(map[string]any{
		"model": fakeModelJSON(),
		"input": userInput("hi"),
		"extensions": []any{map[string]any{
			"provider": "fake",
			"id":       "generate_options",
			"fields":   map[string]any{"cache_ky": "typo"},
		}},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("typo fields error = %v, want validation-classified", err)
	}
}

func TestInferenceBridge_Extensions_MissingIdentity(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Runtime(t), nil,
		WithExtensionDecoder("fake", "generate_options", testExtensionDecoder()))

	_, err := api.generate(map[string]any{
		"model": fakeModelJSON(),
		"input": userInput("hi"),
		"extensions": []any{map[string]any{
			"provider": "fake",
			"fields":   map[string]any{},
		}},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("missing id error = %v, want validation-classified", err)
	}
}

func TestInferenceBridge_Route_Extensions(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	runtime := fake.Runtime(t)
	api := newInferenceAPI(t, runtime, fakeRouter(t, runtime),
		WithExtensionDecoder("fake", "generate_options", testExtensionDecoder()))

	_, err := api.route(map[string]any{
		"input": userInput("hi"),
		"extensions": []any{map[string]any{
			"provider": "fake",
			"id":       "generate_options",
			"fields":   map[string]any{"cache_key": "sess-2"},
		}},
	})
	if err != nil {
		t.Fatalf("route with extensions: %v", err)
	}
	req := fake.LastRequest()
	if len(req.Extensions) != 1 {
		t.Fatalf("router forwarded %+v, want one extension", req.Extensions)
	}
	if ext, ok := req.Extensions[0].(testExtension); !ok || ext.CacheKey != "sess-2" {
		t.Fatalf("extension = %+v (%T), want cache_key sess-2", req.Extensions[0], req.Extensions[0])
	}
}

func TestInferenceBridge_Extensions_NonPointerFactory(t *testing.T) {
	// A value-type factory satisfies the constraint at compile time
	// but cannot be decoded into; the decoder must fail as a host
	// wiring error, not a confusing script-facing validation error.
	decoder := ExtensionDecoderFor(func() testExtension { return testExtension{} })
	_, err := decoder(json.RawMessage(`{}`))
	if err == nil || !errdefs.IsInternal(err) {
		t.Fatalf("non-pointer factory error = %v, want internal-classified", err)
	}
}

func TestInferenceBridge_Stream_EventSequenceAndResult(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Events: []inference.GenerateStreamEvent{
			{PartIndex: 0, Delta: inference.TextPartDelta{Text: "hel"}},
			{PartIndex: 0, Delta: inference.TextPartDelta{Text: "lo"}},
			{FinishReason: inference.FinishCompleted},
		},
	}
	api := newInferenceAPI(t, fake.Runtime(t), nil)

	raw, err := api.stream(map[string]any{
		"model": fakeModelJSON(),
		"input": userInput("hi"),
	})
	if err != nil {
		t.Fatalf("stream open: %v", err)
	}
	s := openStream(t, raw)

	first, err := s.next()
	if err != nil {
		t.Fatalf("next 1: %v", err)
	}
	if delta := first.(map[string]any)["delta"].(map[string]any)["text"]; delta != "hel" {
		t.Fatalf("event 1 delta = %v, want %q", first, "hel")
	}
	second, err := s.next()
	if err != nil {
		t.Fatalf("next 2: %v", err)
	}
	if delta := second.(map[string]any)["delta"].(map[string]any)["text"]; delta != "lo" {
		t.Fatalf("event 2 delta = %v, want %q", second, "lo")
	}
	finish, err := s.next()
	if err != nil {
		t.Fatalf("next 3: %v", err)
	}
	if finish.(map[string]any)["finish_reason"] != string(inference.FinishCompleted) {
		t.Fatalf("event 3 = %v, want finish_reason %q", finish, inference.FinishCompleted)
	}
	if ev, err := s.next(); err != nil || ev != nil {
		t.Fatalf("EOF next = (%v, %v), want (nil, nil)", ev, err)
	}
	if ev, err := s.next(); err != nil || ev != nil {
		t.Fatalf("post-EOF next should stay exhausted, got (%v, %v)", ev, err)
	}

	// The accumulated result is the exact shape generate() returns.
	out, err := s.result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	resp := out.(map[string]any)
	parts := resp["message"].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("accumulated message = %v, want one text part %q", parts, "hello")
	}
	if resp["finish_reason"] != string(inference.FinishCompleted) {
		t.Fatalf("result finish_reason = %v", resp["finish_reason"])
	}
}

func TestInferenceBridge_Stream_ResultBeforeEOF(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Runtime(t), nil)

	raw, err := api.stream(map[string]any{"model": fakeModelJSON(), "input": userInput("hi")})
	if err != nil {
		t.Fatalf("stream open: %v", err)
	}
	s := openStream(t, raw)
	if _, err := s.result(); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("early result error = %v, want validation-classified", err)
	}
}

func TestInferenceBridge_Stream_CloseIdempotent(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Runtime(t), nil)

	raw, err := api.stream(map[string]any{"model": fakeModelJSON(), "input": userInput("hi")})
	if err != nil {
		t.Fatalf("stream open: %v", err)
	}
	s := openStream(t, raw)
	if err := s.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.close(); err != nil {
		t.Fatalf("second close should be a no-op, got %v", err)
	}
	if ev, err := s.next(); err != nil || ev != nil {
		t.Fatalf("next after close = (%v, %v), want (nil, nil)", ev, err)
	}
}

func TestInferenceBridge_Stream_NoRuntime(t *testing.T) {
	api := newInferenceAPI(t, nil, nil)
	_, err := api.stream(map[string]any{"model": fakeModelJSON(), "input": userInput("hi")})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired stream error = %v, want NotAvailable", err)
	}
	_, err = api.routeStream(map[string]any{"input": userInput("hi")})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired routeStream error = %v, want NotAvailable", err)
	}
}

func TestInferenceBridge_RouteStream_TraceOnResult(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Events: []inference.GenerateStreamEvent{
			{PartIndex: 0, Delta: inference.TextPartDelta{Text: "hi"}},
			{FinishReason: inference.FinishCompleted},
		},
	}
	runtime := fake.Runtime(t)
	api := newInferenceAPI(t, runtime, fakeRouter(t, runtime))

	raw, err := api.routeStream(map[string]any{"input": userInput("hi")})
	if err != nil {
		t.Fatalf("routeStream open: %v", err)
	}
	s := openStream(t, raw)
	for {
		ev, err := s.next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if ev == nil {
			break
		}
	}
	out, err := s.result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	resp := out.(map[string]any)
	trace, ok := resp["trace"].(map[string]any)
	if !ok {
		t.Fatalf("routeStream result lacks trace: %v", resp)
	}
	if executed := trace["executed"].(map[string]any)["id"].(map[string]any); executed["name"] != "echo" {
		t.Fatalf("trace.executed = %v, want echo", trace["executed"])
	}
}
