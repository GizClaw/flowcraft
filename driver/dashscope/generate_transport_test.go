package dashscope

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

func testGenerateOperations(
	t *testing.T,
	platform *fakePlatform,
	name string,
) inference.GenerateOperations {
	t.Helper()
	operations, err := openGenerate(
		&clients{media: testClients(t, platform)},
		Spec{},
		testDeclaration(t, name),
		model.ModelID{Provider: providerID, Name: name},
		"",
	)
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}
	return operations
}

// TestTransportGenerateUnary drives one unary call end to end: the request
// the driver posts, and the response it assembles, including the search
// record and the trace's verification stamp.
func TestTransportGenerateUnary(t *testing.T) {
	platform := &fakePlatform{
		generate: func(body map[string]any) (int, string) {
			if body["model"] != "qwen3.7-max" {
				t.Errorf("model = %v", body["model"])
			}
			parameters, ok := body["parameters"].(map[string]any)
			if !ok || parameters["result_format"] != "message" {
				t.Errorf("parameters = %#v", body["parameters"])
			}
			input, ok := body["input"].(map[string]any)
			if !ok || input["messages"] == nil {
				t.Errorf("input = %#v", body["input"])
			}
			return http.StatusOK, `{"output":{"choices":[{"finish_reason":"stop",` +
				`"message":{"role":"assistant","content":"hello there",` +
				`"reasoning_content":"weighing"}}],` +
				`"search_info":{"search_results":[{"title":"Docs",` +
				`"url":"https://example.com/docs","site_name":"example"}]}},` +
				`"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15,` +
				`"prompt_tokens_details":{"cached_tokens":3},` +
				`"output_tokens_details":{"reasoning_tokens":2}},` +
				`"request_id":"req-77"}`
		},
	}
	operations := testGenerateOperations(t, platform, "qwen3.7-max")
	request := userRequest(
		[]message.Part{message.TextPart{Text: "hi"}},
		inference.Intent{Text: &inference.TextIntent{}},
	)
	response, err := operations.Unary.Execute(
		context.Background(), testModelRef("qwen3.7-max"), request)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(response.Message.Content.Parts) != 2 {
		t.Fatalf("parts = %#v", response.Message.Content.Parts)
	}
	reasoning, ok := response.Message.Content.Parts[0].(message.ReasoningPart)
	if !ok || reasoning.Text != "weighing" {
		t.Fatalf("reasoning = %#v", response.Message.Content.Parts[0])
	}
	if reasoning.Source != "dashscope/qwen3.7-max" {
		t.Errorf("reasoning source = %q", reasoning.Source)
	}
	if text, ok := response.Message.Content.Parts[1].(message.TextPart); !ok ||
		text.Text != "hello there" {
		t.Errorf("text = %#v", response.Message.Content.Parts[1])
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Errorf("finish = %q", response.FinishReason)
	}
	usage := response.Usage
	if usage.InputTokens != 11 || usage.OutputTokens != 4 || usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", usage)
	}
	if usage.Input.CacheReadTokens == nil || *usage.Input.CacheReadTokens != 3 ||
		usage.Input.UncachedTokens == nil || *usage.Input.UncachedTokens != 8 {
		t.Errorf("input buckets = %+v", usage.Input)
	}
	if usage.Output.ReasoningTokens == nil || *usage.Output.ReasoningTokens != 2 ||
		usage.Output.ReasoningAccounting != inference.ReasoningIncludedInOutput {
		t.Errorf("output buckets = %+v", usage.Output)
	}
	if response.Metadata.RequestID != "req-77" {
		t.Errorf("request id = %q", response.Metadata.RequestID)
	}
	if len(response.ProviderOutputs) != 1 {
		t.Fatalf("provider outputs = %#v", response.ProviderOutputs)
	}
	search, ok := response.ProviderOutputs[0].(*SearchOutput)
	if !ok || len(search.Citations) != 1 ||
		search.Citations[0].URL != "https://example.com/docs" ||
		search.Citations[0].Title != "Docs" {
		t.Errorf("search output = %#v", response.ProviderOutputs[0])
	}
}

// TestTransportGenerateStream drives the event stream end to end: fragments
// fold into one text part and one reasoning part, the terminal frame carries
// the accounting, and the stream closes on its marker.
func TestTransportGenerateStream(t *testing.T) {
	platform := &fakePlatform{
		stream: func(body map[string]any) (int, string) {
			parameters, ok := body["parameters"].(map[string]any)
			if !ok || parameters["incremental_output"] != true {
				t.Errorf("stream parameters = %#v", body["parameters"])
			}
			frames := []string{
				`data: {"output":{"choices":[{"finish_reason":"null",` +
					`"message":{"role":"assistant","content":"Hel"}}]},` +
					`"usage":{"input_tokens":9,"output_tokens":1,"total_tokens":10},` +
					`"request_id":"req-88"}`,
				`data: {"output":{"choices":[{"finish_reason":"null",` +
					`"message":{"content":"lo","reasoning_content":"weigh"}}]},` +
					`"usage":{"input_tokens":9,"output_tokens":3,"total_tokens":12},` +
					`"request_id":"req-88"}`,
				`data: {"output":{"choices":[{"finish_reason":"stop",` +
					`"message":{"content":""}}]},` +
					`"usage":{"input_tokens":9,"output_tokens":5,"total_tokens":14,` +
					`"output_tokens_details":{"reasoning_tokens":2}},` +
					`"request_id":"req-88"}`,
				`data: [DONE]`,
			}
			return http.StatusOK, strings.Join(frames, "\n\n") + "\n\n"
		},
	}
	operations := testGenerateOperations(t, platform, "qwen3.7-max")
	if operations.Stream == nil {
		t.Fatal("the text surface opened no stream driver")
	}
	request := userRequest(
		[]message.Part{message.TextPart{Text: "hi"}},
		inference.Intent{Text: &inference.TextIntent{}},
	)
	ctx := context.Background()
	stream, err := operations.Stream.Stream(ctx, testModelRef("qwen3.7-max"), request)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var events []inference.GenerateStreamEvent
	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		events = append(events, event)
	}
	if len(events) != 4 {
		t.Fatalf("events = %#v", events)
	}
	first, ok := events[0].Delta.(inference.TextPartDelta)
	if !ok || first.Text != "Hel" || events[0].PartIndex != 0 {
		t.Errorf("event 0 = %#v", events[0])
	}
	second, ok := events[1].Delta.(inference.ReasoningDelta)
	if !ok || second.Text != "weigh" || events[1].PartIndex != 1 {
		t.Errorf("event 1 = %#v", events[1])
	}
	third, ok := events[2].Delta.(inference.TextPartDelta)
	if !ok || third.Text != "lo" || events[2].PartIndex != 0 {
		t.Errorf("event 2 = %#v", events[2])
	}
	if events[3].FinishReason != inference.FinishCompleted ||
		events[3].Usage == nil || events[3].Usage.TotalTokens != 14 {
		t.Errorf("terminal event = %#v", events[3])
	}

	result, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(result.Message.Content.Parts) != 2 {
		t.Fatalf("parts = %#v", result.Message.Content.Parts)
	}
	// Parts keep arrival order: the text fragment opened part 0, the trace
	// arrived next as part 1.
	if text, ok := result.Message.Content.Parts[0].(message.TextPart); !ok ||
		text.Text != "Hello" {
		t.Errorf("text part = %#v", result.Message.Content.Parts[0])
	}
	if reasoning, ok := result.Message.Content.Parts[1].(message.ReasoningPart); !ok ||
		reasoning.Text != "weigh" || reasoning.Source != "dashscope/qwen3.7-max" {
		t.Errorf("reasoning part = %#v", result.Message.Content.Parts[1])
	}
	if result.Metadata.RequestID != "req-88" {
		t.Errorf("request id = %q", result.Metadata.RequestID)
	}
	calls := platform.recorded()
	if len(calls) != 1 || calls[0].sse != "enable" {
		t.Errorf("recorded calls = %#v", calls)
	}
}

// TestTransportGenerateInBandError pins that a refusal reported inside an
// HTTP 200 body classifies like a transport failure instead of surfacing as
// an empty success.
func TestTransportGenerateInBandError(t *testing.T) {
	platform := &fakePlatform{
		generate: func(map[string]any) (int, string) {
			return http.StatusOK, `{"code":"Throttling.RateQuota",` +
				`"message":"Requests rate limit exceeded",` +
				`"request_id":"req-99"}`
		},
	}
	operations := testGenerateOperations(t, platform, "qwen3.7-max")
	request := userRequest(
		[]message.Part{message.TextPart{Text: "hi"}},
		inference.Intent{Text: &inference.TextIntent{}},
	)
	_, err := operations.Unary.Execute(
		context.Background(), testModelRef("qwen3.7-max"), request)
	if err == nil {
		t.Fatal("the in-band refusal compiled as a success")
	}
	if !errdefs.IsRateLimit(err) {
		t.Errorf("classification = %v", err)
	}
	if requestID, ok := errdefs.RequestID(err); !ok || requestID != "req-99" {
		t.Errorf("request id = %q, %v", requestID, ok)
	}
}

// TestTransportGenerateStreamError pins that a refusal frame inside the event
// stream fails the read with the platform's classification.
func TestTransportGenerateStreamError(t *testing.T) {
	platform := &fakePlatform{
		stream: func(map[string]any) (int, string) {
			return http.StatusOK, "data: {\"code\":\"InvalidParameter\"," +
				"\"message\":\"check the model name\"," +
				"\"request_id\":\"req-100\"}\n\n"
		},
	}
	operations := testGenerateOperations(t, platform, "qwen3.7-max")
	request := userRequest(
		[]message.Part{message.TextPart{Text: "hi"}},
		inference.Intent{Text: &inference.TextIntent{}},
	)
	ctx := context.Background()
	stream, err := operations.Stream.Stream(ctx, testModelRef("qwen3.7-max"), request)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	_, err = stream.Next(ctx)
	if err == nil {
		t.Fatal("the refusal frame read as an event")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("classification = %v", err)
	}
}
