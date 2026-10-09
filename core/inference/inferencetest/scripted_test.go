package inferencetest_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
)

const scriptedChatBody = `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`

// postChat sends one chat completions request and returns the response.
func postChat(t *testing.T, ctx context.Context, srv *inferencetest.ScriptedOpenAI, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL()+"/chat/completions", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

type scriptedChatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func decodeChatResponse(t *testing.T, resp *http.Response) scriptedChatResponse {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var out scriptedChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func TestScriptedOpenAIUnary(t *testing.T) {
	srv := inferencetest.NewScriptedOpenAI(t,
		inferencetest.ScriptedReply{Text: "hello there"},
	)
	resp, err := postChat(t, context.Background(), srv, scriptedChatBody)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	out := decodeChatResponse(t, resp)
	if out.ID != "chatcmpl-fake" {
		t.Fatalf("id = %q, want chatcmpl-fake", out.ID)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	if got := out.Choices[0].Message.Content; got != "hello there" {
		t.Fatalf("content = %q, want hello there", got)
	}
	if got := out.Choices[0].FinishReason; got != "stop" {
		t.Fatalf("finish = %q, want stop", got)
	}
	if out.Usage == nil || out.Usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v, want total 15", out.Usage)
	}

	if got := srv.Calls(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	messages, err := srv.LastMessages()
	if err != nil {
		t.Fatalf("LastMessages: %v", err)
	}
	if len(messages) != 1 || messages[0]["content"] != "hi" {
		t.Fatalf("messages = %+v, want the user turn", messages)
	}

	// A wrong path is a 404, not a silently served completion.
	bad, err := http.Get(srv.URL() + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bad.Body.Close() }()
	if bad.StatusCode != http.StatusNotFound {
		t.Fatalf("bad path status = %d, want 404", bad.StatusCode)
	}
}

func TestScriptedOpenAIRepliesInOrderAndRepeatsLast(t *testing.T) {
	srv := inferencetest.NewScriptedOpenAI(t,
		inferencetest.ScriptedReply{Text: "first"},
		inferencetest.ScriptedReply{Text: "second"},
	)
	for i, want := range []string{"first", "second", "second"} {
		resp, err := postChat(t, context.Background(), srv, scriptedChatBody)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		out := decodeChatResponse(t, resp)
		if got := out.Choices[0].Message.Content; got != want {
			t.Fatalf("call %d content = %q, want %q", i, got, want)
		}
	}
	all, err := srv.MessagesForCalls()
	if err != nil {
		t.Fatalf("MessagesForCalls: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("recorded %d request bodies, want 3", len(all))
	}
}

func TestScriptedOpenAIToolCalls(t *testing.T) {
	srv := inferencetest.NewScriptedOpenAI(t,
		inferencetest.ScriptedReply{ToolCalls: []inferencetest.ScriptedToolCall{
			{Name: "read_file", Arguments: `{"path":"a.txt"}`},
		}},
	)
	resp, err := postChat(t, context.Background(), srv, scriptedChatBody)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	out := decodeChatResponse(t, resp)
	choice := out.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", choice.Message.ToolCalls)
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "call_0_1" || call.Function.Name != "read_file" ||
		call.Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("tool call = %+v", call)
	}
}

func TestScriptedOpenAIErrorInjection(t *testing.T) {
	srv := inferencetest.NewScriptedOpenAI(t,
		inferencetest.ScriptedReply{Status: http.StatusTooManyRequests, Error: "rate limited"},
	)
	resp, err := postChat(t, context.Background(), srv, scriptedChatBody)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if out.Error.Message != "rate limited" || out.Error.Type == "" {
		t.Fatalf("error body = %+v, want the OpenAI-shaped error", out.Error)
	}
}

func TestScriptedOpenAIWithoutUsage(t *testing.T) {
	srv := inferencetest.NewScriptedOpenAI(t,
		inferencetest.ScriptedReply{Text: "no usage"},
	).WithoutUsage()
	resp, err := postChat(t, context.Background(), srv, scriptedChatBody)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := out["usage"]; ok {
		t.Fatalf("usage present: %+v", out["usage"])
	}
}

func TestScriptedOpenAIStreamFrames(t *testing.T) {
	srv := inferencetest.NewScriptedOpenAI(t,
		inferencetest.ScriptedReply{Text: "he", RequestID: "req_scripted_1"},
	)
	resp, err := postChat(t, context.Background(), srv,
		`{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("x-request-id"); got != "req_scripted_1" {
		t.Fatalf("x-request-id = %q, want req_scripted_1", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	frames := strings.Split(strings.TrimSpace(string(raw)), "\n\n")
	if len(frames) != 5 {
		t.Fatalf("frames = %d, want role+content+stop+usage+[DONE]:\n%s", len(frames), raw)
	}
	if frames[len(frames)-1] != "data: [DONE]" {
		t.Fatalf("last frame = %q, want data: [DONE]", frames[len(frames)-1])
	}
	var text strings.Builder
	var finish string
	var usageTotal int
	for _, frame := range frames[:len(frames)-1] {
		payload, ok := strings.CutPrefix(frame, "data: ")
		if !ok {
			t.Fatalf("frame is not SSE data: %q", frame)
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", payload, err)
		}
		for _, choice := range chunk.Choices {
			text.WriteString(choice.Delta.Content)
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
		if chunk.Usage != nil {
			usageTotal = chunk.Usage.TotalTokens
		}
	}
	if text.String() != "he" || finish != "stop" || usageTotal != 15 {
		t.Fatalf("stream text=%q finish=%q usage=%d, want he/stop/15",
			text.String(), finish, usageTotal)
	}
}

func TestScriptedOpenAIHoldGate(t *testing.T) {
	srv := inferencetest.NewScriptedOpenAI(t,
		inferencetest.ScriptedReply{Text: "late"},
		inferencetest.ScriptedReply{Text: "never delivered"},
	)
	gate := srv.HoldNext()
	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := postChat(t, context.Background(), srv, scriptedChatBody)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		done <- result{body: string(raw), err: err}
	}()
	<-gate.Ready()
	if got := srv.Calls(); got != 1 {
		t.Fatalf("calls = %d, want the held request recorded before release", got)
	}
	gate.Release()
	select {
	case r := <-done:
		if r.err != nil || !strings.Contains(r.body, "late") {
			t.Fatalf("held request = (%q, %v), want the released reply", r.body, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held request did not complete after Release")
	}

	// A held request can be abandoned by the caller's deadline; releasing
	// the gate afterwards lets the handler finish for a clean shutdown.
	gate = srv.HoldNext()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := postChat(t, ctx, srv, scriptedChatBody); err == nil {
		// A completed request would mean the hold never took; it must not
		// fail silently even if the transport somehow wins the race.
		t.Fatal("held request should fail on the client deadline")
	}
	gate.Release()
}
