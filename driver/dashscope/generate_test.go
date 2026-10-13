package dashscope

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func intPtr(value int) *int           { return &value }
func int64Ptr(value int64) *int64     { return &value }
func floatPtr(value float64) *float64 { return &value }
func boolPtr(value bool) *bool        { return &value }

// TestCompileGenerateTextSurface pins the classic surface's payload: string
// content, result_format forced to the message shape, the declared
// max_completion_tokens lowering, and the tool and response controls.
func TestCompileGenerateTextSurface(t *testing.T) {
	declared := testDeclaration(t, "qwen3.7-max")
	request := userRequest(
		[]message.Part{message.TextPart{Text: "summarize the brief"}},
		inference.Intent{Text: &inference.TextIntent{
			MaxOutputTokens: intPtr(128),
			Temperature:     floatPtr(0.3),
			TopP:            floatPtr(0.9),
			Tools: []message.ToolDefinition{{
				Name:        "lookup",
				Description: "look something up",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			ToolChoice: &inference.ToolChoice{
				Kind: inference.ToolChoiceNamed,
				Name: "lookup",
			},
			Response: &inference.ResponseFormat{
				Kind: inference.ResponseJSONObject,
			},
		}},
	)
	request.Context = []message.Message{{
		Role: message.RoleSystem,
		Content: message.Content{
			Parts: []message.Part{message.TextPart{Text: "be brief"}},
		},
	}}

	wire, report, err := compileGenerateWire(
		t, declared, request, inference.GenerateExecutionUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.path != generatePathText || wire.model != "qwen3.7-max" {
		t.Fatalf("wire path/model = %q %q", wire.path, wire.model)
	}
	if wire.stream {
		t.Error("unary compile marked the wire as streaming")
	}
	if wire.params["result_format"] != "message" {
		t.Errorf("result_format = %v", wire.params["result_format"])
	}
	if _, ok := wire.params["incremental_output"]; ok {
		t.Error("unary compile asked for incremental output")
	}
	if wire.params["max_completion_tokens"] != 128 {
		t.Errorf("max_completion_tokens = %v", wire.params["max_completion_tokens"])
	}
	if _, ok := wire.params["max_tokens"]; ok {
		t.Error("the declared lowering also sent max_tokens")
	}
	if wire.params["temperature"] != 0.3 || wire.params["top_p"] != 0.9 {
		t.Errorf("sampling = %v %v",
			wire.params["temperature"], wire.params["top_p"])
	}
	if len(wire.messages) != 2 {
		t.Fatalf("messages = %#v", wire.messages)
	}
	if wire.messages[0]["role"] != "system" ||
		wire.messages[0]["content"] != "be brief" {
		t.Errorf("system message = %#v", wire.messages[0])
	}
	if wire.messages[1]["role"] != "user" ||
		wire.messages[1]["content"] != "summarize the brief" {
		t.Errorf("user message = %#v", wire.messages[1])
	}
	tools, ok := wire.params["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", wire.params["tools"])
	}
	function, ok := tools[0]["function"].(map[string]any)
	if !ok || function["name"] != "lookup" ||
		string(function["parameters"].(json.RawMessage)) != `{"type":"object"}` {
		t.Errorf("tool function = %#v", tools[0]["function"])
	}
	choice, ok := wire.params["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice = %#v", wire.params["tool_choice"])
	}
	if named, _ := choice["function"].(map[string]any); named["name"] != "lookup" {
		t.Errorf("tool_choice = %#v", choice)
	}
	format, ok := wire.params["response_format"].(map[string]any)
	if !ok || format["type"] != "json_object" {
		t.Errorf("response_format = %#v", wire.params["response_format"])
	}
	for _, field := range []inference.FieldID{
		inference.FieldGenerateIntentTools,
		inference.FieldGenerateIntentToolChoice,
		inference.FieldGenerateIntentTemperature,
		inference.FieldGenerateIntentTextMaxOutputTokens,
	} {
		if reason := rejectedReason(report, field); reason != "" {
			t.Errorf("%s rejected: %s", field, reason)
		}
	}
}

// TestCompileGenerateMultimodalContent pins the Qwen3.5+ surface's content
// array: typed entries in order, with inline image bytes as a data URI.
func TestCompileGenerateMultimodalContent(t *testing.T) {
	declared := testDeclaration(t, "qwen3.8-max")
	inline, err := media.NewImageBytes([]byte("png-bytes"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "what is in this clip"},
			message.ImagePart{Source: inline},
			videoPart(t, "https://cdn.example.com/clip.mp4"),
			audioPart(t, "https://cdn.example.com/track.wav"),
		},
		inference.Intent{Text: &inference.TextIntent{}},
	)

	wire, _, err := compileGenerateWire(
		t, declared, request, inference.GenerateExecutionUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.path != generatePathMultimodal {
		t.Fatalf("wire path = %q", wire.path)
	}
	if _, ok := wire.params["result_format"]; ok {
		t.Error("the multimodal surface carried result_format")
	}
	content, ok := wire.messages[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content = %#v", wire.messages[0]["content"])
	}
	if len(content) != 4 {
		t.Fatalf("content = %#v", content)
	}
	if content[0]["text"] != "what is in this clip" {
		t.Errorf("content[0] = %#v", content[0])
	}
	wantImage := "data:image/png;base64," +
		base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	if content[1]["image"] != wantImage {
		t.Errorf("content[1] = %#v", content[1])
	}
	if content[2]["video"] != "https://cdn.example.com/clip.mp4" {
		t.Errorf("content[2] = %#v", content[2])
	}
	if content[3]["audio"] != "https://cdn.example.com/track.wav" {
		t.Errorf("content[3] = %#v", content[3])
	}
}

// TestCompileGenerateMediaRejections covers both sides of the media rules:
// the text surface carries no media at all, and the multimodal surface
// carries only the kinds the model declares.
func TestCompileGenerateMediaRejections(t *testing.T) {
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "describe"},
			imagePart(t, "https://cdn.example.com/a.png"),
		},
		inference.Intent{Text: &inference.TextIntent{}},
	)
	_, report, err := compileGenerateWire(
		t, testDeclaration(t, "qwen3.7-max"), request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("the text surface accepted an image part")
	}
	if reason := rejectedReason(report, inference.FieldGenerateInputImage); !strings.Contains(reason, "text-generation surface") {
		t.Errorf("image reason = %q", reason)
	}

	declared := testDeclaration(t, "qwen3.8-max")
	declared.Capabilities.Inputs = []message.PartKind{message.PartText}
	_, report, err = compileGenerateWire(
		t, declared, request, inference.GenerateExecutionUnary)
	if err == nil {
		t.Fatal("a model without image input accepted an image part")
	}
	if reason := rejectedReason(report, inference.FieldGenerateInputImage); !strings.Contains(reason, "does not accept image input") {
		t.Errorf("image reason = %q", reason)
	}
}

// TestCompileGenerateStreamShape pins what each surface adds when the
// execution shape is streaming: incremental output for both, the stream
// parameter for the multimodal surface only.
func TestCompileGenerateStreamShape(t *testing.T) {
	for _, tc := range []struct {
		model        string
		streamParam  bool
		resultFormat bool
	}{
		{model: "qwen3.7-max", resultFormat: true},
		{model: "qwen3.8-max", streamParam: true},
	} {
		request := userRequest(
			[]message.Part{message.TextPart{Text: "hello"}},
			inference.Intent{Text: &inference.TextIntent{}},
		)
		wire, _, err := compileGenerateWire(
			t, testDeclaration(t, tc.model), request,
			inference.GenerateExecutionStream,
		)
		if err != nil {
			t.Fatalf("%s: compile: %v", tc.model, err)
		}
		if !wire.stream || wire.params["incremental_output"] != true {
			t.Errorf("%s: stream params = %#v", tc.model, wire.params)
		}
		if _, ok := wire.params["stream"]; ok != tc.streamParam {
			t.Errorf("%s: stream parameter = %v", tc.model, wire.params["stream"])
		}
		if _, ok := wire.params["result_format"]; ok != tc.resultFormat {
			t.Errorf("%s: result_format present = %v", tc.model, ok)
		}
	}
}

// TestCompileGenerateReasoning pins the thinking dial: the toggle rides
// enable_thinking, effort has no level dial and drops with the budget hint,
// and the budget rides the options extension.
func TestCompileGenerateReasoning(t *testing.T) {
	declared := testDeclaration(t, "qwen3.7-max")
	request := userRequest(
		[]message.Part{message.TextPart{Text: "why"}},
		inference.Intent{Text: &inference.TextIntent{
			ReasoningEnabled: boolPtr(false),
		}},
		GenerateOptions{Provider: providerID, ThinkingBudget: intPtr(1024)},
	)
	_, report, err := compileGenerateWire(
		t, declared, request, inference.GenerateExecutionUnary)
	if err == nil {
		t.Fatal("a thinking budget compiled while thinking was disabled")
	}
	if reason := rejectedReason(
		report, generateOptionsField("thinking_budget")); !strings.Contains(reason, "thinking is disabled") {
		t.Errorf("budget reason = %q", reason)
	}

	request = userRequest(
		[]message.Part{message.TextPart{Text: "why"}},
		inference.Intent{Text: &inference.TextIntent{
			ReasoningEnabled: boolPtr(false),
			ReasoningEffort:  model.ReasoningHigh,
		}},
	)
	wire, report, err := compileGenerateWire(
		t, declared, request, inference.GenerateExecutionUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.params["enable_thinking"] != false {
		t.Errorf("enable_thinking = %v", wire.params["enable_thinking"])
	}
	decision, ok := decisionFor(report, inference.FieldGenerateIntentReasoningEffort)
	if !ok || decision.Disposition != inference.Dropped {
		t.Fatalf("effort decision = %+v", decision)
	}
	if !strings.Contains(decision.Reason, "thinking_budget") {
		t.Errorf("effort reason = %q", decision.Reason)
	}

	always := testDeclaration(t, "qwen3.8-max")
	request = userRequest(
		[]message.Part{message.TextPart{Text: "why"}},
		inference.Intent{Text: &inference.TextIntent{
			ReasoningEnabled: boolPtr(false),
		}},
	)
	_, report, err = compileGenerateWire(
		t, always, request, inference.GenerateExecutionUnary)
	if err == nil {
		t.Fatal("an always-thinking model accepted reasoning_enabled=false")
	}
	if reason := rejectedReason(
		report, inference.FieldGenerateIntentReasoningEnabled); !strings.Contains(reason, "cannot disable thinking") {
		t.Errorf("always reason = %q", reason)
	}
}

// TestCompileGenerateSearch pins the hosted search gate: an undeclared model
// rejects the knob, a declared one lowers it with its options.
func TestCompileGenerateSearch(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "today's news"}},
		inference.Intent{Text: &inference.TextIntent{}},
		GenerateOptions{Provider: providerID, EnableSearch: boolPtr(true)},
	)
	_, report, err := compileGenerateWire(
		t, testDeclaration(t, "qwen3.8-max"), request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("a model without hosted search accepted enable_search")
	}
	if reason := rejectedReason(
		report, generateOptionsField("enable_search")); !strings.Contains(reason, "hosted web search") {
		t.Errorf("search reason = %q", reason)
	}

	request = userRequest(
		[]message.Part{message.TextPart{Text: "today's news"}},
		inference.Intent{Text: &inference.TextIntent{}},
		GenerateOptions{
			Provider:     providerID,
			EnableSearch: boolPtr(true),
			SearchOptions: &SearchOptions{
				Strategy:       "agent",
				EnableCitation: boolPtr(true),
			},
		},
	)
	wire, _, err := compileGenerateWire(
		t, testDeclaration(t, "qwen3.7-max"), request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.params["enable_search"] != true {
		t.Errorf("enable_search = %v", wire.params["enable_search"])
	}
	search, ok := wire.params["search_options"].(map[string]any)
	if !ok || search["strategy"] != "agent" || search["enable_citation"] != true {
		t.Errorf("search_options = %#v", wire.params["search_options"])
	}

	request = userRequest(
		[]message.Part{message.TextPart{Text: "today's news"}},
		inference.Intent{Text: &inference.TextIntent{}},
		GenerateOptions{
			Provider:      providerID,
			SearchOptions: &SearchOptions{Strategy: "turbo"},
		},
	)
	_, report, err = compileGenerateWire(
		t, testDeclaration(t, "qwen3.7-max"), request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("search_options compiled without enable_search")
	}
	if reason := rejectedReason(
		report, generateOptionsField("search_options")); !strings.Contains(reason, "apply only while web search is enabled") {
		t.Errorf("options reason = %q", reason)
	}
}

// TestCompileGenerateContinuation pins the tool continuation shape and the
// reasoning replay rules: the trace rides back only where the declaration and
// the verification scope both allow it.
func TestCompileGenerateContinuation(t *testing.T) {
	toolTurn := func(source string) inference.GenerateRequest {
		request := userRequest(
			[]message.Part{message.TextPart{Text: "and now?"}},
			inference.Intent{Text: &inference.TextIntent{}},
		)
		request.Context = []message.Message{
			{
				Role: message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{
					message.ReasoningPart{Text: "weighing", Source: source},
					message.ToolCallPart{Call: message.ToolCall{
						ID:        "call-1",
						Name:      "lookup",
						Arguments: json.RawMessage(`{"q":"x"}`),
					}},
				}},
			},
			{
				Role: message.RoleTool,
				Content: message.Content{Parts: []message.Part{
					message.ToolResultPart{Result: message.ToolResult{
						CallID: "call-1",
						Content: message.Content{Parts: []message.Part{
							message.TextPart{Text: "42"},
						}},
					}},
				}},
			},
		}
		return request
	}

	replay := testDeclaration(t, "qwen3.8-max")
	wire, _, err := compileGenerateWire(
		t, replay, toolTurn("dashscope/qwen3.8-max"),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(wire.messages) != 3 {
		t.Fatalf("messages = %#v", wire.messages)
	}
	assistant := wire.messages[0]
	if assistant["reasoning_content"] != "weighing" {
		t.Errorf("assistant reasoning = %#v", assistant["reasoning_content"])
	}
	calls, ok := assistant["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 || calls[0]["id"] != "call-1" {
		t.Fatalf("tool_calls = %#v", assistant["tool_calls"])
	}
	if _, ok := assistant["content"]; ok {
		t.Errorf("a call-only assistant turn carried content: %#v", assistant)
	}
	tool := wire.messages[1]
	if tool["role"] != "tool" || tool["content"] != "42" ||
		tool["tool_call_id"] != "call-1" {
		t.Errorf("tool message = %#v", tool)
	}

	// A trace from another scope is not replayed: the target cannot verify
	// it, and the platform would refuse the whole turn.
	_, report, err := compileGenerateWire(
		t, replay, toolTurn("other/scope"), inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	decision, ok := decisionFor(report, inference.FieldGenerateContextReasoning)
	if !ok || decision.Disposition != inference.Dropped {
		t.Fatalf("foreign trace decision = %+v", decision)
	}
	if !strings.Contains(decision.Reason, "another verification scope") {
		t.Errorf("foreign trace reason = %q", decision.Reason)
	}

	// The classic surface does not consume traces at all without the
	// declaration.
	_, report, err = compileGenerateWire(
		t, testDeclaration(t, "qwen3.7-max"), toolTurn("dashscope/qwen3.7-max"),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	decision, ok = decisionFor(report, inference.FieldGenerateContextReasoning)
	if !ok || decision.Disposition != inference.Dropped {
		t.Fatalf("default trace decision = %+v", decision)
	}
	if !strings.Contains(decision.Reason, "replay_reasoning") {
		t.Errorf("default trace reason = %q", decision.Reason)
	}
}

// TestCompileGenerateIntentRejections pins that a non-text intent rejects on
// its own field rather than compiling a text request that ignores it.
func TestCompileGenerateIntentRejections(t *testing.T) {
	request := userRequest(
		[]message.Part{message.TextPart{Text: "draw"}},
		inference.Intent{Image: &inference.ImageIntent{}},
	)
	_, report, err := compileGenerateWire(
		t, testDeclaration(t, "qwen3.7-max"), request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("an image intent compiled on a text model")
	}
	if reason := rejectedReason(
		report, inference.FieldGenerateIntentImage); !strings.Contains(reason, "Wan image model") {
		t.Errorf("image intent reason = %q", reason)
	}

	request = userRequest(
		[]message.Part{message.TextPart{Text: "say hi"}},
		inference.Intent{Text: &inference.TextIntent{}},
		GenerateOptions{Provider: providerID, VlHighResolutionImages: boolPtr(true)},
	)
	_, report, err = compileGenerateWire(
		t, testDeclaration(t, "qwen3.7-max"), request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("a text-only model accepted vl_high_resolution_images")
	}
	if reason := rejectedReason(
		report, generateOptionsField("vl_high_resolution_images")); !strings.Contains(reason, "does not accept image input") {
		t.Errorf("resolution reason = %q", reason)
	}
}
