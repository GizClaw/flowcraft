package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
)

func buildProvider(t *testing.T, spec map[string]any) inference.ProviderDefinition {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := config.NewSecret([]byte("test-key"))
	if err != nil {
		t.Fatal(err)
	}
	provider, err := Factory().Build(context.Background(), config.ProviderInput{
		ID:   "deepseek",
		Spec: raw,
		Profiles: []config.ResolvedProfile{{
			ID:      "default",
			Secrets: map[string]config.Secret{SecretAPIKey: secret},
		}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return provider
}

func newTestRuntime(t *testing.T, server *chatServer) *inference.Runtime {
	t.Helper()
	provider := buildProvider(t, map[string]any{"base_url": server.URL})
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{provider})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

// decisionOf finds the compiler report's disposition for one field on a
// response's metadata.
func decisionOf(response inference.GenerateResponse, field inference.FieldID) inference.Decision {
	for _, decision := range response.Metadata.Decisions {
		if decision.Field == field {
			return decision
		}
	}
	return inference.Decision{}
}

func TestGenerateUnaryReasoning(t *testing.T) {
	server := newChatServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, chatCompletionJSON("stop", map[string]any{
			"reasoning_content": "thinking aloud",
		}))
	})
	runtime := newTestRuntime(t, server)

	response, err := runtime.Generate(context.Background(), generateModel("deepseek-v4-pro"), simpleTextRequest("hi"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d (%#v)", len(parts), parts)
	}
	reasoning, ok := parts[0].(message.ReasoningPart)
	if !ok || reasoning.Text != "thinking aloud" {
		t.Fatalf("part[0] = %#v", parts[0])
	}
	if _, ok := parts[1].(message.TextPart); !ok {
		t.Fatalf("part[1] = %#v", parts[1])
	}
	if response.Usage.Input.CacheReadTokens == nil || *response.Usage.Input.CacheReadTokens != 3 {
		t.Fatalf("cache read = %+v", response.Usage.Input)
	}
	if response.Usage.Output.ReasoningTokens == nil || *response.Usage.Output.ReasoningTokens != 2 {
		t.Fatalf("reasoning tokens = %+v", response.Usage.Output)
	}
	if response.Usage.Output.ReasoningAccounting != inference.ReasoningIncludedInOutput {
		t.Fatalf("accounting = %q", response.Usage.Output.ReasoningAccounting)
	}
}

func TestGenerateStreamReasoningThenText(t *testing.T) {
	server := newChatServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody(
			reasoningChunk("think"),
			reasoningChunk("ing"),
			textChunk("an"),
			textChunk("swer"),
			finishChunk("stop"),
			usageChunk(),
		))
	})
	runtime := newTestRuntime(t, server)

	stream, err := runtime.GenerateStream(context.Background(), generateModel("deepseek-v4-pro"), simpleTextRequest("hi"))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer stream.Close()

	type delta struct {
		part int
		kind string
		text string
	}
	var deltas []delta
	var finish inference.GenerateStreamEvent
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch value := event.Delta.(type) {
		case inference.ReasoningDelta:
			deltas = append(deltas, delta{part: event.PartIndex, kind: "reasoning", text: value.Text})
		case inference.TextPartDelta:
			deltas = append(deltas, delta{part: event.PartIndex, kind: "text", text: value.Text})
		}
		if event.FinishReason != "" {
			finish = event
		}
	}

	want := []delta{
		{part: 0, kind: "reasoning", text: "think"},
		{part: 0, kind: "reasoning", text: "ing"},
		{part: 1, kind: "text", text: "an"},
		{part: 1, kind: "text", text: "swer"},
	}
	if fmt.Sprintf("%v", deltas) != fmt.Sprintf("%v", want) {
		t.Fatalf("deltas = %v, want %v", deltas, want)
	}
	if finish.FinishReason != inference.FinishCompleted {
		t.Fatalf("finish = %q", finish.FinishReason)
	}
	if finish.Usage == nil || finish.Usage.TotalTokens != 19 {
		t.Fatalf("usage = %+v", finish.Usage)
	}
}

func TestGenerateStreamToolCalls(t *testing.T) {
	server := newChatServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody(
			toolCallDeltaChunk(0, "call_9", "lookup", ""),
			toolCallDeltaChunk(0, "", "", `{"q":`),
			toolCallDeltaChunk(0, "", "", `"ark"}`),
			finishChunk("tool_calls"),
			usageChunk(),
		))
	})
	runtime := newTestRuntime(t, server)

	request := simpleTextRequest("find something")
	request.Input.Content.Intent.Text.Tools = []message.Definition{toolCallDefinition()}

	stream, err := runtime.GenerateStream(context.Background(), generateModel("deepseek-v4-pro"), request)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer stream.Close()

	var id, name, args string
	var finish inference.FinishReason
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if fragment, ok := event.Delta.(inference.ToolCallDelta); ok {
			if fragment.ID != "" {
				id = fragment.ID
			}
			if fragment.Name != "" {
				name = fragment.Name
			}
			args += fragment.ArgumentsFragment
		}
		if event.FinishReason != "" {
			finish = event.FinishReason
		}
	}
	if id != "call_9" || name != "lookup" || args != `{"q":"ark"}` {
		t.Fatalf("call = %q %q %s", id, name, args)
	}
	if finish != inference.FinishToolCalls {
		t.Fatalf("finish = %q", finish)
	}
}

// A tool-calling assistant turn carries its reasoning back natively; a
// tool-calling turn without a trace still carries the field, empty,
// because DeepSeek 400s the request otherwise.
func TestReasoningRoundTripPolicy(t *testing.T) {
	server := newChatServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, chatCompletionJSON("stop", nil))
	})
	runtime := newTestRuntime(t, server)

	request := simpleTextRequest("again")
	request.Context = []message.Message{
		{
			Role: message.RoleUser,
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "find something"},
			}},
		},
		{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.ReasoningPart{Text: "trace"},
				message.ToolCallPart{Call: message.Call{
					ID:        "call_9",
					Name:      "lookup",
					Arguments: json.RawMessage(`{"q":"ark"}`),
				}},
			}},
		},
		{
			Role: message.RoleTool,
			Content: message.Content{Parts: []message.Part{
				message.ToolResultPart{Result: message.Result{CallID: "call_9", Content: "found it"}},
			}},
		},
	}

	response, err := runtime.Generate(context.Background(), generateModel("deepseek-v4-pro"), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	decision := decisionOf(response, inference.FieldGenerateContextReasoning)
	if decision.Disposition != inference.Native {
		t.Fatalf("reasoning disposition = %q (%q)", decision.Disposition, decision.Reason)
	}

	messages, _ := server.captured()["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("wire messages = %d", len(messages))
	}
	assistant, _ := messages[1].(map[string]any)
	if assistant["reasoning_content"] != "trace" {
		t.Fatalf("reasoning_content = %v", assistant["reasoning_content"])
	}
	toolMessage, _ := messages[2].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "call_9" {
		t.Fatalf("tool message = %v", toolMessage)
	}

	// Same turn without the trace: the field must still ride, empty.
	request.Context[1].Content.Parts = request.Context[1].Content.Parts[1:]
	if _, err := runtime.Generate(context.Background(), generateModel("deepseek-v4-pro"), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	messages, _ = server.captured()["messages"].([]any)
	assistant, _ = messages[1].(map[string]any)
	field, exists := assistant["reasoning_content"]
	if !exists || field != "" {
		t.Fatalf("reasoning_content = %v (exists %v)", field, exists)
	}
}

func TestThinkingAndEffortOverrides(t *testing.T) {
	server := newChatServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, chatCompletionJSON("stop", nil))
	})
	runtime := newTestRuntime(t, server)

	// Default: neither field rides — the API default (thinking enabled)
	// decides.
	if _, err := runtime.Generate(context.Background(), generateModel("deepseek-v4-pro"), simpleTextRequest("hi")); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	captured := server.captured()
	if _, exists := captured["thinking"]; exists {
		t.Fatalf("default request carried thinking: %v", captured["thinking"])
	}
	if _, exists := captured["reasoning_effort"]; exists {
		t.Fatalf("default request carried reasoning_effort: %v", captured["reasoning_effort"])
	}

	// Canonical switch opt-out plus effort passthrough.
	disabled := false
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text.ReasoningEnabled = &disabled
	if _, err := runtime.Generate(context.Background(), generateModel("deepseek-v4-pro"), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	thinking, _ := server.captured()["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking = %v", thinking)
	}

	request = simpleTextRequest("hi")
	request.Input.Content.Intent.Text.ReasoningEffort = inference.ReasoningHigh
	if _, err := runtime.Generate(context.Background(), generateModel("deepseek-v4-pro"), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if server.captured()["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", server.captured()["reasoning_effort"])
	}
}

func TestInsufficientSystemResource(t *testing.T) {
	server := newChatServer(t, func(w http.ResponseWriter, request map[string]any) {
		if request["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, sseBody(textChunk("partial"), finishChunk("insufficient_system_resource")))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, chatCompletionJSON("insufficient_system_resource", nil))
	})
	runtime := newTestRuntime(t, server)

	_, err := runtime.Generate(context.Background(), generateModel("deepseek-v4-pro"), simpleTextRequest("hi"))
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unary err = %v", err)
	}

	stream, err := runtime.GenerateStream(context.Background(), generateModel("deepseek-v4-pro"), simpleTextRequest("hi"))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer stream.Close()
	for {
		_, err = stream.Next(context.Background())
		if err != nil {
			break
		}
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("stream err = %v", err)
	}
}

func TestStreamWireCarriesStreamOptions(t *testing.T) {
	server := newChatServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody(textChunk("ok"), finishChunk("stop"), usageChunk()))
	})
	runtime := newTestRuntime(t, server)

	stream, err := runtime.GenerateStream(context.Background(), generateModel("deepseek-v4-pro"), simpleTextRequest("hi"))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Next(context.Background()); err != nil {
			break
		}
	}

	captured := server.captured()
	if captured["stream"] != true {
		t.Fatalf("stream = %v", captured["stream"])
	}
	options, _ := captured["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("stream_options = %v", options)
	}
	if !strings.Contains(string(mustJSON(t, captured["messages"])), "hi") {
		t.Fatalf("messages = %v", captured["messages"])
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
