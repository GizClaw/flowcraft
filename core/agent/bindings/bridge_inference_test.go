package bindings

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/message"
)

// The inference bridge projects wire JSON straight into Runtime/Router
// calls, so the tests run real Runtime and Router instances over the
// canned provider in inferencetest — no provider I/O, but the full
// resolve/compile/validate pipeline is exercised.

func fakeRouter(t *testing.T, runtime *inference.Assembly) *route.Router {
	t.Helper()
	router, err := route.New(runtime, route.Selectors{
		Generate: inferencetest.StaticGenerateSelector(inferencetest.DefaultFakeModel),
	})
	if err != nil {
		t.Fatalf("route.New: %v", err)
	}
	return router
}

func fakeEmbedRouter(t *testing.T, runtime *inference.Assembly) *route.Router {
	t.Helper()
	router, err := route.New(runtime, route.Selectors{
		Embed: inferencetest.StaticEmbedSelector(inferencetest.DefaultFakeEmbedModel),
	})
	if err != nil {
		t.Fatalf("route.New: %v", err)
	}
	return router
}

func fakeTranscribeRouter(t *testing.T, runtime *inference.Assembly) *route.Router {
	t.Helper()
	router, err := route.New(runtime, route.Selectors{
		Transcribe:        inferencetest.StaticTranscribeSelector(inferencetest.DefaultFakeTranscribeModel),
		TranscribeSession: inferencetest.StaticTranscribeSessionSelector(inferencetest.DefaultFakeTranscribeModel),
	})
	if err != nil {
		t.Fatalf("route.New: %v", err)
	}
	return router
}

type inferenceAPI struct {
	generate               func(raw any) (any, error)
	route                  func(raw any) (any, error)
	explain                func(raw any) (any, error)
	routeExplain           func(raw any) (any, error)
	models                 func() (any, error)
	inspect                func(raw any) (any, error)
	explainStream          func(raw any) (any, error)
	routeExplainStream     func(raw any) (any, error)
	embed                  func(raw any) (any, error)
	routeEmbed             func(raw any) (any, error)
	explainEmbed           func(raw any) (any, error)
	routeExplainEmbed      func(raw any) (any, error)
	stream                 func(raw any) (any, error)
	routeStream            func(raw any) (any, error)
	transcribe             func(raw any) (any, error)
	routeTranscribe        func(raw any) (any, error)
	explainTranscribe      func(raw any) (any, error)
	routeExplainTranscribe func(raw any) (any, error)
	transcribeSession      func(raw any) (any, error)
	routeTranscribeSession func(raw any) (any, error)
}

func newInferenceAPI(t *testing.T, runtime *inference.Assembly, router *route.Router, opts ...InferenceBridgeOption) inferenceAPI {
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
	explainFn, ok := m["explain"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.explain = %T", m["explain"])
	}
	routeExplainFn, ok := m["routeExplain"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.routeExplain = %T", m["routeExplain"])
	}
	modelsFn, ok := m["models"].(func() (any, error))
	if !ok {
		t.Fatalf("inference.models = %T", m["models"])
	}
	inspectFn, ok := m["inspect"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.inspect = %T", m["inspect"])
	}
	explainStreamFn, ok := m["explainStream"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.explainStream = %T", m["explainStream"])
	}
	routeExplainStreamFn, ok := m["routeExplainStream"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.routeExplainStream = %T", m["routeExplainStream"])
	}
	embedFn, ok := m["embed"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.embed = %T", m["embed"])
	}
	routeEmbedFn, ok := m["routeEmbed"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.routeEmbed = %T", m["routeEmbed"])
	}
	explainEmbedFn, ok := m["explainEmbed"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.explainEmbed = %T", m["explainEmbed"])
	}
	routeExplainEmbedFn, ok := m["routeExplainEmbed"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.routeExplainEmbed = %T", m["routeExplainEmbed"])
	}
	streamFn, ok := m["stream"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.stream = %T", m["stream"])
	}
	routeStreamFn, ok := m["routeStream"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.routeStream = %T", m["routeStream"])
	}
	transcribeFn, ok := m["transcribe"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.transcribe = %T", m["transcribe"])
	}
	routeTranscribeFn, ok := m["routeTranscribe"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.routeTranscribe = %T", m["routeTranscribe"])
	}
	explainTranscribeFn, ok := m["explainTranscribe"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.explainTranscribe = %T", m["explainTranscribe"])
	}
	routeExplainTranscribeFn, ok := m["routeExplainTranscribe"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.routeExplainTranscribe = %T", m["routeExplainTranscribe"])
	}
	transcribeSessionFn, ok := m["transcribeSession"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.transcribeSession = %T", m["transcribeSession"])
	}
	routeTranscribeSessionFn, ok := m["routeTranscribeSession"].(func(any) (any, error))
	if !ok {
		t.Fatalf("inference.routeTranscribeSession = %T", m["routeTranscribeSession"])
	}
	return inferenceAPI{
		generate:               generate,
		route:                  routeFn,
		explain:                explainFn,
		routeExplain:           routeExplainFn,
		models:                 modelsFn,
		inspect:                inspectFn,
		explainStream:          explainStreamFn,
		routeExplainStream:     routeExplainStreamFn,
		embed:                  embedFn,
		routeEmbed:             routeEmbedFn,
		explainEmbed:           explainEmbedFn,
		routeExplainEmbed:      routeExplainEmbedFn,
		stream:                 streamFn,
		routeStream:            routeStreamFn,
		transcribe:             transcribeFn,
		routeTranscribe:        routeTranscribeFn,
		explainTranscribe:      explainTranscribeFn,
		routeExplainTranscribe: routeExplainTranscribeFn,
		transcribeSession:      transcribeSessionFn,
		routeTranscribeSession: routeTranscribeSessionFn,
	}
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

// sessionHandle mirrors the script-facing transcription session handle.
type sessionHandle struct {
	send      func(any) error
	next      func() (any, error)
	result    func() (any, error)
	interrupt func() error
	close     func() error
}

func openSession(t *testing.T, raw any) sessionHandle {
	t.Helper()
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("session handle = %T, want map[string]any", raw)
	}
	send, ok := m["send"].(func(any) error)
	if !ok {
		t.Fatalf("session.send = %T", m["send"])
	}
	next, ok := m["next"].(func() (any, error))
	if !ok {
		t.Fatalf("session.next = %T", m["next"])
	}
	result, ok := m["result"].(func() (any, error))
	if !ok {
		t.Fatalf("session.result = %T", m["result"])
	}
	interrupt, ok := m["interrupt"].(func() error)
	if !ok {
		t.Fatalf("session.interrupt = %T", m["interrupt"])
	}
	closeFn, ok := m["close"].(func() error)
	if !ok {
		t.Fatalf("session.close = %T", m["close"])
	}
	return sessionHandle{
		send:      send,
		next:      next,
		result:    result,
		interrupt: interrupt,
		close:     closeFn,
	}
}

func fakeModelJSON() map[string]any {
	return map[string]any{
		"id":      map[string]any{"provider": "fake", "name": "echo"},
		"profile": "default",
	}
}

func fakeTranscribeModelJSON() map[string]any {
	return map[string]any{
		"id":      map[string]any{"provider": "fake", "name": "transcribe"},
		"profile": "default",
	}
}

func transcribeAudioJSON() map[string]any {
	return map[string]any{
		"kind":       "inline",
		"data":       []byte{1, 2, 3, 4},
		"media_type": "audio/wav",
	}
}

func transcribeSessionJSON() map[string]any {
	return map[string]any{
		"input_format": map[string]any{
			"encoding":       "pcm16",
			"sample_rate_hz": 16000,
			"channels":       1,
		},
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

func embedItem(text string) map[string]any {
	return map[string]any{
		"content": map[string]any{
			"parts": []any{map[string]any{"type": "text", "text": text}},
		},
	}
}

func TestInferenceBridge_Generate_RoundTrip(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

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
	msg, ok := resp["message"].(map[string]any)
	if !ok {
		t.Fatalf("message = %T, want object", resp["message"])
	}
	parts, ok := msg["content"].(map[string]any)["parts"].([]any)
	if !ok || len(parts) != 1 || parts[0].(map[string]any)["text"] != "ok" {
		t.Fatalf("message parts = %v, want one text part %q", msg["content"], "ok")
	}

	// The canonical request reached the provider intact: context plus
	// the current-turn input, in order.
	reqs := fake.Requests()
	if len(reqs) != 1 || len(reqs[0].Context) != 1 || len(reqs[0].Context[0].Content.Parts) != 1 {
		t.Fatalf("provider saw context = %+v, want one message", reqs)
	}
	text, ok := reqs[0].Input.Content.Parts[0].(message.TextPart)
	if !ok || text.Text != "hi" || reqs[0].Input.Role != inference.InputRoleUser {
		t.Fatalf("provider saw input = %+v, want user text %q", reqs[0].Input, "hi")
	}
}

func TestInferenceBridge_Generate_MissingModel(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	_, err := api.generate(map[string]any{"input": userInput("hi")})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("missing model error = %v, want validation-classified", err)
	}
}

func TestInferenceBridge_Generate_StrictUnknownField(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

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
	api := newInferenceAPI(t, fake.Assembly(t), nil)

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
	runtime := fake.Assembly(t)
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
	runtime := fake.Assembly(t)
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
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	_, err := api.route(map[string]any{"input": userInput("hi")})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired route error = %v, want NotAvailable", err)
	}
}

func TestInferenceBridge_Explain_RoundTrip(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	out, err := api.explain(map[string]any{
		"model": fakeModelJSON(),
		"input": userInput("hi"),
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	explanation, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("explanation = %T, want object", out)
	}
	if explanation["Operation"] != string(inference.OperationGenerate) {
		t.Fatalf("explanation.Operation = %v, want %q",
			explanation["Operation"], inference.OperationGenerate)
	}
	model, ok := explanation["Model"].(map[string]any)
	if !ok || model["id"] == nil {
		t.Fatalf("explanation.Model = %v, want model identity", explanation["Model"])
	}
	// Explain compiles locally: the compiler ran, but no transport
	// execution produced a response.
	if reqs := fake.Requests(); len(reqs) != 1 {
		t.Fatalf("explain compiled requests = %d, want 1", len(reqs))
	}
}

func TestInferenceBridge_Explain_NoRuntime(t *testing.T) {
	api := newInferenceAPI(t, nil, nil)
	_, err := api.explain(map[string]any{
		"model": fakeModelJSON(),
		"input": userInput("hi"),
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired explain error = %v, want NotAvailable", err)
	}
}

func TestInferenceBridge_RouteExplain_RoundTrip(t *testing.T) {
	limit := 128_000
	fake := &inferencetest.GenerateFake{
		Descriptor: inference.ModelDescriptor{
			ID: inferencetest.DefaultFakeModel.ID,
			Limits: inference.ModelLimits{
				MaxInputTokens: &limit,
			},
		},
	}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeRouter(t, runtime))

	out, err := api.routeExplain(map[string]any{"input": userInput("hi")})
	if err != nil {
		t.Fatalf("routeExplain: %v", err)
	}
	obj, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("routeExplain result = %T, want object", out)
	}
	if _, ok := obj["explanation"].(map[string]any); !ok {
		t.Fatalf("routeExplain.explanation = %T, want object", obj["explanation"])
	}
	decision, ok := obj["decision"].(map[string]any)
	if !ok {
		t.Fatalf("routeExplain.decision = %T, want object", obj["decision"])
	}
	selected, ok := decision["selected"].(map[string]any)
	if !ok || selected["id"] == nil {
		t.Fatalf("routeExplain.decision.selected = %v, want model identity", decision["selected"])
	}
	limits, ok := obj["limits"].(map[string]any)
	if !ok {
		t.Fatalf("routeExplain.limits = %T, want object", obj["limits"])
	}
	if limits["max_input_tokens"] != float64(limit) {
		t.Fatalf("routeExplain.limits.max_input_tokens = %v, want %d",
			limits["max_input_tokens"], limit)
	}
	// One compile for the routed preflight; no transport execution.
	if reqs := fake.Requests(); len(reqs) != 1 {
		t.Fatalf("routeExplain compiled requests = %d, want 1", len(reqs))
	}
}

func TestInferenceBridge_RouteExplain_RejectsModelKey(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeRouter(t, runtime))

	_, err := api.routeExplain(map[string]any{
		"model": fakeModelJSON(),
		"input": userInput("hi"),
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("routeExplain with model key error = %v, want validation-classified", err)
	}
}

func TestInferenceBridge_RouteExplain_NoRouter(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	_, err := api.routeExplain(map[string]any{"input": userInput("hi")})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired routeExplain error = %v, want NotAvailable", err)
	}
}

func TestInferenceBridge_Models_RoundTrip(t *testing.T) {
	limit := 128_000
	fake := &inferencetest.GenerateFake{
		Descriptor: inference.ModelDescriptor{
			ID: inferencetest.DefaultFakeModel.ID,
			Limits: inference.ModelLimits{
				MaxInputTokens: &limit,
			},
		},
	}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	out, err := api.models()
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	models, ok := out.([]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %T (%v), want one descriptor", out, out)
	}
	descriptor, ok := models[0].(map[string]any)
	if !ok {
		t.Fatalf("model descriptor = %T, want object", models[0])
	}
	limits, ok := descriptor["limits"].(map[string]any)
	if !ok || limits["max_input_tokens"] != float64(limit) {
		t.Fatalf("descriptor limits = %v, want max_input_tokens %d", descriptor["limits"], limit)
	}
}

func TestInferenceBridge_Models_NoRuntime(t *testing.T) {
	api := newInferenceAPI(t, nil, nil)
	_, err := api.models()
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired models error = %v, want NotAvailable", err)
	}
}

func TestInferenceBridge_Inspect_RoundTrip(t *testing.T) {
	limit := 64_000
	fake := &inferencetest.GenerateFake{
		Descriptor: inference.ModelDescriptor{
			ID: inferencetest.DefaultFakeModel.ID,
			Limits: inference.ModelLimits{
				MaxInputTokens: &limit,
			},
		},
	}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	out, err := api.inspect(fakeModelJSON())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	descriptor, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("descriptor = %T, want object", out)
	}
	limits, ok := descriptor["limits"].(map[string]any)
	if !ok || limits["max_input_tokens"] != float64(limit) {
		t.Fatalf("descriptor limits = %v, want max_input_tokens %d", descriptor["limits"], limit)
	}
}

func TestInferenceBridge_Inspect_UnknownModel(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	_, err := api.inspect(map[string]any{
		"id": map[string]any{"provider": "fake", "name": "ghost"},
	})
	if err == nil {
		t.Fatal("inspect of unknown model should fail")
	}
}

func TestInferenceBridge_ExplainStream_RoundTrip(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	out, err := api.explainStream(map[string]any{
		"model": fakeModelJSON(),
		"input": userInput("hi"),
	})
	if err != nil {
		t.Fatalf("explainStream: %v", err)
	}
	explanation, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("explanation = %T, want object", out)
	}
	if explanation["Operation"] != string(inference.OperationGenerate) {
		t.Fatalf("explanation.Operation = %v, want %q",
			explanation["Operation"], inference.OperationGenerate)
	}
}

func TestInferenceBridge_RouteExplainStream_RoundTrip(t *testing.T) {
	limit := 128_000
	fake := &inferencetest.GenerateFake{
		Descriptor: inference.ModelDescriptor{
			ID: inferencetest.DefaultFakeModel.ID,
			Limits: inference.ModelLimits{
				MaxInputTokens: &limit,
			},
		},
	}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeRouter(t, runtime))

	out, err := api.routeExplainStream(map[string]any{"input": userInput("hi")})
	if err != nil {
		t.Fatalf("routeExplainStream: %v", err)
	}
	obj, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("routeExplainStream result = %T, want object", out)
	}
	if _, ok := obj["explanation"].(map[string]any); !ok {
		t.Fatalf("routeExplainStream.explanation = %T, want object", obj["explanation"])
	}
	decision, ok := obj["decision"].(map[string]any)
	if !ok || decision["selected"] == nil {
		t.Fatalf("routeExplainStream.decision = %v, want selected model", obj["decision"])
	}
	limits, ok := obj["limits"].(map[string]any)
	if !ok || limits["max_input_tokens"] != float64(limit) {
		t.Fatalf("routeExplainStream.limits = %v, want max_input_tokens %d",
			obj["limits"], limit)
	}
}

func TestInferenceBridge_Embed_RoundTrip(t *testing.T) {
	fake := &inferencetest.EmbedFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	out, err := api.embed(map[string]any{
		"model": map[string]any{
			"id":      map[string]any{"provider": "fake", "name": "embed"},
			"profile": "default",
		},
		"items": []any{embedItem("hello")},
	})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	resp, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("embed response = %T, want object", out)
	}
	embeddings, ok := resp["embeddings"].([]any)
	if !ok || len(embeddings) != 1 {
		t.Fatalf("embeddings = %v, want one vector", resp["embeddings"])
	}
}

func TestInferenceBridge_Embed_NoRuntime(t *testing.T) {
	api := newInferenceAPI(t, nil, nil)
	_, err := api.embed(map[string]any{
		"model": map[string]any{
			"id": map[string]any{"provider": "fake", "name": "embed"},
		},
		"items": []any{embedItem("hello")},
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired embed error = %v, want NotAvailable", err)
	}
}

func TestInferenceBridge_RouteEmbed_RoundTrip(t *testing.T) {
	fake := &inferencetest.EmbedFake{}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeEmbedRouter(t, runtime))

	out, err := api.routeEmbed(map[string]any{
		"items": []any{embedItem("hello")},
	})
	if err != nil {
		t.Fatalf("routeEmbed: %v", err)
	}
	resp, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("routeEmbed response = %T, want object", out)
	}
	if _, ok := resp["embeddings"].([]any); !ok {
		t.Fatalf("embeddings = %T, want array", resp["embeddings"])
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
	if !ok || id["provider"] != "fake" || id["name"] != "embed" {
		t.Fatalf("trace.executed.id = %v, want fake/embed", executed["id"])
	}
}

func TestInferenceBridge_RouteEmbed_RejectsModelKey(t *testing.T) {
	fake := &inferencetest.EmbedFake{}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeEmbedRouter(t, runtime))

	_, err := api.routeEmbed(map[string]any{
		"model": map[string]any{
			"id": map[string]any{"provider": "fake", "name": "embed"},
		},
		"items": []any{embedItem("hello")},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("routeEmbed with model key error = %v, want validation-classified", err)
	}
}

func TestInferenceBridge_ExplainEmbed_RoundTrip(t *testing.T) {
	fake := &inferencetest.EmbedFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	out, err := api.explainEmbed(map[string]any{
		"model": map[string]any{
			"id":      map[string]any{"provider": "fake", "name": "embed"},
			"profile": "default",
		},
		"items": []any{embedItem("hello")},
	})
	if err != nil {
		t.Fatalf("explainEmbed: %v", err)
	}
	explanation, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("explanation = %T, want object", out)
	}
	if explanation["Operation"] != string(inference.OperationEmbed) {
		t.Fatalf("explanation.Operation = %v, want %q",
			explanation["Operation"], inference.OperationEmbed)
	}
}

func TestInferenceBridge_RouteExplainEmbed_RoundTrip(t *testing.T) {
	limit := 8_192
	fake := &inferencetest.EmbedFake{
		Descriptor: inference.ModelDescriptor{
			ID: inferencetest.DefaultFakeEmbedModel.ID,
			Limits: inference.ModelLimits{
				MaxInputTokens: &limit,
			},
		},
	}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeEmbedRouter(t, runtime))

	out, err := api.routeExplainEmbed(map[string]any{
		"items": []any{embedItem("hello")},
	})
	if err != nil {
		t.Fatalf("routeExplainEmbed: %v", err)
	}
	obj, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("routeExplainEmbed result = %T, want object", out)
	}
	if _, ok := obj["explanation"].(map[string]any); !ok {
		t.Fatalf("routeExplainEmbed.explanation = %T, want object", obj["explanation"])
	}
	decision, ok := obj["decision"].(map[string]any)
	if !ok || decision["selected"] == nil {
		t.Fatalf("routeExplainEmbed.decision = %v, want selected model", obj["decision"])
	}
	limits, ok := obj["limits"].(map[string]any)
	if !ok || limits["max_input_tokens"] != float64(limit) {
		t.Fatalf("routeExplainEmbed.limits = %v, want max_input_tokens %d",
			obj["limits"], limit)
	}
}

func TestInferenceBridge_Generate_ToolCallResponse(t *testing.T) {
	// The multi-turn contract: a tool_calls finish carries the
	// assistant message verbatim, tool_call id included, so the script
	// can forward it to tools.callAll and continue with role="tool".
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{
						message.ToolCallPart{Call: message.ToolCall{
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
	api := newInferenceAPI(t, fake.Assembly(t), nil)

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

func testExtensionDecoder() inference.ExtensionDecoder {
	return inference.ExtensionDecoderFor(func() *testExtension { return &testExtension{} })
}

func TestInferenceBridge_Generate_Extensions(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil,
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
	api := newInferenceAPI(t, fake.Assembly(t), nil,
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
	api := newInferenceAPI(t, fake.Assembly(t), nil,
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

func TestInferenceBridge_Transcribe_RoundTrip(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	out, err := api.transcribe(map[string]any{
		"model": fakeTranscribeModelJSON(),
		"audio": transcribeAudioJSON(),
	})
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	resp, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("transcribe response = %T, want object", out)
	}
	if resp["text"] != "ok" {
		t.Fatalf("text = %v, want %q", resp["text"], "ok")
	}
}

func TestInferenceBridge_Transcribe_NoRuntime(t *testing.T) {
	api := newInferenceAPI(t, nil, nil)
	_, err := api.transcribe(map[string]any{
		"model": fakeTranscribeModelJSON(),
		"audio": transcribeAudioJSON(),
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired transcribe error = %v, want NotAvailable", err)
	}
}

func TestInferenceBridge_RouteTranscribe_RoundTrip(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeTranscribeRouter(t, runtime))

	out, err := api.routeTranscribe(map[string]any{
		"audio": transcribeAudioJSON(),
	})
	if err != nil {
		t.Fatalf("routeTranscribe: %v", err)
	}
	resp, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("routeTranscribe response = %T, want object", out)
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
	if !ok || id["provider"] != "fake" || id["name"] != "transcribe" {
		t.Fatalf("trace.executed.id = %v, want fake/transcribe", executed["id"])
	}
}

func TestInferenceBridge_RouteTranscribe_RejectsModelKey(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeTranscribeRouter(t, runtime))

	_, err := api.routeTranscribe(map[string]any{
		"model": fakeTranscribeModelJSON(),
		"audio": transcribeAudioJSON(),
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("routeTranscribe with model key error = %v, want validation-classified", err)
	}
}

func TestInferenceBridge_Transcribe_Extensions(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil,
		WithExtensionDecoder("fake", "generate_options", testExtensionDecoder()))

	_, err := api.transcribe(map[string]any{
		"model": fakeTranscribeModelJSON(),
		"audio": transcribeAudioJSON(),
		"extensions": []any{map[string]any{
			"provider": "fake",
			"id":       "generate_options",
			"fields":   map[string]any{"cache_key": "sess-1"},
		}},
	})
	if err != nil {
		t.Fatalf("transcribe with extensions: %v", err)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 || len(reqs[0].Extensions) != 1 {
		t.Fatalf("provider saw %+v, want one extension", reqs)
	}
	ext, ok := reqs[0].Extensions[0].(testExtension)
	if !ok || ext.CacheKey != "sess-1" {
		t.Fatalf("extension = %+v (%T), want cache_key sess-1", reqs[0].Extensions[0], reqs[0].Extensions[0])
	}
}

func TestInferenceBridge_RouteTranscribe_Extensions(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeTranscribeRouter(t, runtime),
		WithExtensionDecoder("fake", "generate_options", testExtensionDecoder()))

	_, err := api.routeTranscribe(map[string]any{
		"audio": transcribeAudioJSON(),
		"extensions": []any{map[string]any{
			"provider": "fake",
			"id":       "generate_options",
			"fields":   map[string]any{"cache_key": "sess-2"},
		}},
	})
	if err != nil {
		t.Fatalf("routeTranscribe with extensions: %v", err)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 || len(reqs[0].Extensions) != 1 {
		t.Fatalf("router forwarded %+v, want one extension", reqs)
	}
	if ext, ok := reqs[0].Extensions[0].(testExtension); !ok || ext.CacheKey != "sess-2" {
		t.Fatalf("extension = %+v (%T), want cache_key sess-2", reqs[0].Extensions[0], reqs[0].Extensions[0])
	}
}

func TestInferenceBridge_TranscribeSession_Extensions(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil,
		WithExtensionDecoder("fake", "generate_options", testExtensionDecoder()))

	_, err := api.transcribeSession(map[string]any{
		"model":        fakeTranscribeModelJSON(),
		"input_format": transcribeSessionJSON()["input_format"],
		"extensions": []any{map[string]any{
			"provider": "fake",
			"id":       "generate_options",
			"fields":   map[string]any{"cache_key": "sess-3"},
		}},
	})
	if err != nil {
		t.Fatalf("transcribeSession with extensions: %v", err)
	}
	reqs := fake.SessionRequests()
	if len(reqs) != 1 || len(reqs[0].Extensions) != 1 {
		t.Fatalf("provider saw %+v, want one session extension", reqs)
	}
	if ext, ok := reqs[0].Extensions[0].(testExtension); !ok || ext.CacheKey != "sess-3" {
		t.Fatalf("extension = %+v (%T), want cache_key sess-3", reqs[0].Extensions[0], reqs[0].Extensions[0])
	}
}

func TestInferenceBridge_RouteTranscribeSession_Extensions(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeTranscribeRouter(t, runtime),
		WithExtensionDecoder("fake", "generate_options", testExtensionDecoder()))

	raw := transcribeSessionJSON()
	raw["extensions"] = []any{map[string]any{
		"provider": "fake",
		"id":       "generate_options",
		"fields":   map[string]any{"cache_key": "sess-4"},
	}}
	_, err := api.routeTranscribeSession(raw)
	if err != nil {
		t.Fatalf("routeTranscribeSession with extensions: %v", err)
	}
	// Route session open compiles twice: the Explain preflight and the
	// actual open. Both must carry the extension.
	reqs := fake.SessionRequests()
	if len(reqs) != 2 {
		t.Fatalf("router forwarded %d session requests, want 2", len(reqs))
	}
	for _, req := range reqs {
		if len(req.Extensions) != 1 {
			t.Fatalf("session request extensions = %+v, want one", req.Extensions)
		}
		if ext, ok := req.Extensions[0].(testExtension); !ok || ext.CacheKey != "sess-4" {
			t.Fatalf("extension = %+v (%T), want cache_key sess-4",
				req.Extensions[0], req.Extensions[0])
		}
	}
}

func TestInferenceBridge_ExplainTranscribe_RoundTrip(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	out, err := api.explainTranscribe(map[string]any{
		"model": fakeTranscribeModelJSON(),
		"audio": transcribeAudioJSON(),
	})
	if err != nil {
		t.Fatalf("explainTranscribe: %v", err)
	}
	explanation, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("explanation = %T, want object", out)
	}
	if explanation["Operation"] != string(inference.OperationTranscription) {
		t.Fatalf("explanation.Operation = %v, want %q",
			explanation["Operation"], inference.OperationTranscription)
	}
}

func TestInferenceBridge_RouteExplainTranscribe_RoundTrip(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeTranscribeRouter(t, runtime))

	out, err := api.routeExplainTranscribe(map[string]any{
		"audio": transcribeAudioJSON(),
	})
	if err != nil {
		t.Fatalf("routeExplainTranscribe: %v", err)
	}
	resp, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("routeExplainTranscribe response = %T, want object", out)
	}
	if _, ok := resp["explanation"].(map[string]any); !ok {
		t.Fatalf("explanation = %T, want object", resp["explanation"])
	}
	if _, ok := resp["decision"].(map[string]any); !ok {
		t.Fatalf("decision = %T, want object", resp["decision"])
	}
	if _, ok := resp["limits"].(map[string]any); !ok {
		t.Fatalf("limits = %T, want object", resp["limits"])
	}
}

func TestInferenceBridge_TranscribeSession_RoundTrip(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	out, err := api.transcribeSession(map[string]any{
		"model":        fakeTranscribeModelJSON(),
		"input_format": transcribeSessionJSON()["input_format"],
	})
	if err != nil {
		t.Fatalf("transcribeSession: %v", err)
	}
	handle := openSession(t, out)
	if err := handle.send(map[string]any{
		"data":     []byte{0, 0},
		"sequence": 1,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	for {
		event, err := handle.next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if event == nil {
			break
		}
	}
	result, err := handle.result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	resp, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result = %T, want object", result)
	}
	if resp["text"] != "ok" {
		t.Fatalf("text = %v, want %q", resp["text"], "ok")
	}
	if err := handle.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestInferenceBridge_RouteTranscribeSession_RoundTrip(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	runtime := fake.Assembly(t)
	api := newInferenceAPI(t, runtime, fakeTranscribeRouter(t, runtime))

	out, err := api.routeTranscribeSession(transcribeSessionJSON())
	if err != nil {
		t.Fatalf("routeTranscribeSession: %v", err)
	}
	handle := openSession(t, out)
	if err := handle.send(map[string]any{"data": []byte{0, 0}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	for {
		event, err := handle.next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if event == nil {
			break
		}
	}
	result, err := handle.result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	resp, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result = %T, want object", result)
	}
	if _, ok := resp["trace"].(map[string]any); !ok {
		t.Fatalf("trace missing or %T, want object", resp["trace"])
	}
	if err := handle.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestInferenceBridge_TranscribeSession_Interrupt(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil)

	out, err := api.transcribeSession(map[string]any{
		"model":        fakeTranscribeModelJSON(),
		"input_format": transcribeSessionJSON()["input_format"],
	})
	if err != nil {
		t.Fatalf("transcribeSession: %v", err)
	}
	handle := openSession(t, out)
	if err := handle.send(map[string]any{"data": []byte{0, 0}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := handle.interrupt(); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if _, err := handle.next(); err == nil {
		t.Fatal("next after interrupt succeeded, want interruption error")
	}
	if _, err := handle.result(); err == nil {
		t.Fatal("result after interrupt succeeded, want interruption error")
	}
	if err := handle.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestInferenceBridge_Extensions_MissingIdentity(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	api := newInferenceAPI(t, fake.Assembly(t), nil,
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
	runtime := fake.Assembly(t)
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
	decoder := inference.ExtensionDecoderFor(func() testExtension { return testExtension{} })
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
	api := newInferenceAPI(t, fake.Assembly(t), nil)

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
	api := newInferenceAPI(t, fake.Assembly(t), nil)

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
	api := newInferenceAPI(t, fake.Assembly(t), nil)

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
	runtime := fake.Assembly(t)
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
