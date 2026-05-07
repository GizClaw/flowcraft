package helpers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MockOpenAI starts an httptest.Server that speaks just enough of
// the OpenAI Chat Completions wire format for vesseld's catalog
// `openai` provider to talk to it. We use this instead of polluting
// the daemon's catalog with a "mock" provider — vesseld never
// learns it is talking to a fake; the difference lives entirely in
// the LLMProfile's `baseURL` config.
//
// Returned URL has no trailing slash; the openai sdkx provider
// appends its own paths starting with `/v1/...`.
//
// The mock counts every Generate call in CallCount so tests can
// assert "the dispatch path actually reached the LLM client".
type MockOpenAI struct {
	srv       *httptest.Server
	CallCount atomic.Int64

	// Reply is the assistant message body returned for every
	// chat completion call when no scripted ScriptReply matches.
	// Tests override it before issuing requests when they need
	// a specific response.
	Reply string

	// FailNext, when > 0, causes the next N chat completion
	// requests to return 500. Useful for chaos / retry tests.
	// Decremented after each forced failure.
	FailNext atomic.Int64

	// FailStatus is the HTTP status returned while FailNext is
	// active. Defaults to 500 when zero.
	FailStatus atomic.Int64

	// Delay sleeps for the configured duration before responding.
	// Honoured per-request; tests use it to exercise vessel-side
	// timeouts without orchestrating real network latency.
	Delay atomic.Int64 // nanoseconds (time.Duration as int64)

	// scripted is the FIFO of pre-canned replies. Each chat
	// completion request consumes one entry; once empty the
	// handler falls back to the plain Reply field. Tests use
	// QueueReply to set up multi-turn flows (e.g. "first turn:
	// emit tool_call, second turn: see ToolResult, return text").
	scripted     []ScriptedReply
	scriptedLock sync.Mutex

	// requests captures every chat completion request the server
	// observed. Tests inspect the slice via LastRequest /
	// AllRequests to assert what the LLM was actually shown
	// (system prompt, tool definitions, history). Cleared by
	// ResetRequests.
	requests     []ChatRequest
	requestsLock sync.Mutex

	// authChecker, when non-nil, is run on every chat completion
	// request before the reply is composed. Returns (status,
	// errBody) — non-zero status triggers an immediate error
	// response. Used for "missing api key" / "401" injection.
	AuthChecker func(authHeader string) (int, string)
}

// ScriptedReply is one entry in the FIFO of canned replies. Either
// Content (plain text) or ToolCalls (zero-or-more) drives the
// shape; FinishReason defaults to "stop" / "tool_calls" depending
// on which is set.
type ScriptedReply struct {
	// Content is the assistant text. Ignored when ToolCalls is
	// non-empty (the OpenAI wire format pairs message.content
	// with tool_calls, but the assistant rarely emits both —
	// keeping them mutually exclusive in the mock matches what
	// real models do).
	Content string

	// ToolCalls, when non-empty, makes the reply emit one or
	// more tool_calls and forces FinishReason="tool_calls".
	ToolCalls []ToolCall

	// FinishReason overrides the default. Useful for "length"
	// (token-cap hit) or "content_filter" simulation.
	FinishReason string

	// PromptTokens / CompletionTokens override the usage block.
	// Zero values yield 1/1 — matching the original simple mock.
	PromptTokens     int
	CompletionTokens int

	// Status, when non-zero, causes this reply to be served as a
	// non-200 error response. Use for scripted upstream chaos
	// (e.g. one 429 followed by a successful retry).
	Status int
}

// ToolCall is the OpenAI tool_call shape the mock emits. Name is
// the tool id the engine should invoke; Arguments is the raw JSON
// string the engine passes to the tool's Execute.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ChatRequest is the captured wire request the mock observed. We
// keep only fields tests actually assert on; the full body is also
// retained as Raw for ad-hoc inspection.
type ChatRequest struct {
	Model    string
	Messages []map[string]any
	Tools    []map[string]any
	Headers  http.Header
	Raw      []byte
}

// NewMockOpenAI returns an unstarted-then-started server bound to
// 127.0.0.1 on a random free port. Callers MUST defer Close.
func NewMockOpenAI() *MockOpenAI {
	m := &MockOpenAI{Reply: "ok"}
	mux := http.NewServeMux()
	// The openai-go client treats `baseURL` as the literal API
	// root and appends paths starting at `/chat/completions`,
	// NOT `/v1/chat/completions`. Real OpenAI works because
	// users pass `https://api.openai.com/v1` as the baseURL —
	// the `/v1` is part of the user-supplied root, not the SDK.
	// We register both shapes so a future user passing
	// `baseURL: https://x/v1` also resolves cleanly.
	mux.HandleFunc("/chat/completions", m.handleChat)
	mux.HandleFunc("/v1/chat/completions", m.handleChat)
	emptyModels := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   []any{},
		})
	}
	mux.HandleFunc("/models", emptyModels)
	mux.HandleFunc("/v1/models", emptyModels)
	m.srv = httptest.NewServer(mux)
	return m
}

// URL returns the base URL the LLMProfile config should use as
// `base_url` (a.k.a. `baseURL` in YAML).
func (m *MockOpenAI) URL() string { return m.srv.URL }

// Close shuts the underlying httptest.Server down. Always pair
// with `defer m.Close()` to avoid leaking listeners across tests.
func (m *MockOpenAI) Close() { m.srv.Close() }

// QueueReply appends one or more scripted replies to the FIFO. The
// handler consumes them in order; once exhausted the simple Reply
// field takes over again. Safe to call concurrently.
func (m *MockOpenAI) QueueReply(replies ...ScriptedReply) {
	m.scriptedLock.Lock()
	defer m.scriptedLock.Unlock()
	m.scripted = append(m.scripted, replies...)
}

// QueueText is a sugar for QueueReply with plain-text content.
func (m *MockOpenAI) QueueText(texts ...string) {
	rs := make([]ScriptedReply, len(texts))
	for i, t := range texts {
		rs[i] = ScriptedReply{Content: t}
	}
	m.QueueReply(rs...)
}

// QueueToolCall is a sugar for the common "first turn: dispatch
// to tool X" pattern. Returns the ToolCall id so callers can match
// it in the subsequent ToolResult assertion.
func (m *MockOpenAI) QueueToolCall(name, args string) string {
	id := fmt.Sprintf("call_%d", time.Now().UnixNano())
	m.QueueReply(ScriptedReply{ToolCalls: []ToolCall{{ID: id, Name: name, Arguments: args}}})
	return id
}

// LastRequest returns the most recent captured ChatRequest, or
// (zero, false) when no request has been observed.
func (m *MockOpenAI) LastRequest() (ChatRequest, bool) {
	m.requestsLock.Lock()
	defer m.requestsLock.Unlock()
	if len(m.requests) == 0 {
		return ChatRequest{}, false
	}
	return m.requests[len(m.requests)-1], true
}

// AllRequests returns a snapshot of every captured ChatRequest.
// Useful for multi-turn assertions ("turn 1 saw user msg X; turn
// 2 saw user msg X + tool result Y").
func (m *MockOpenAI) AllRequests() []ChatRequest {
	m.requestsLock.Lock()
	defer m.requestsLock.Unlock()
	out := make([]ChatRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

// ResetRequests clears the captured-requests buffer. Use between
// scenario phases when a single test exercises multiple flows.
func (m *MockOpenAI) ResetRequests() {
	m.requestsLock.Lock()
	defer m.requestsLock.Unlock()
	m.requests = nil
}

// handleChat is the OpenAI Chat Completions handler. Captures the
// request, runs auth/fail injection, then either pops a scripted
// reply or falls back to the simple Reply field.
func (m *MockOpenAI) handleChat(w http.ResponseWriter, r *http.Request) {
	m.CallCount.Add(1)
	defer r.Body.Close()

	if d := m.Delay.Load(); d > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Duration(d)):
		}
	}
	if remaining := m.FailNext.Load(); remaining > 0 {
		m.FailNext.Add(-1)
		status := int(m.FailStatus.Load())
		if status == 0 {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, map[string]any{
			"error": map[string]string{"type": "mock_failure", "message": "scripted upstream failure"},
		})
		return
	}

	var raw map[string]any
	body, _ := readBody(r)
	if err := json.Unmarshal(body, &raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	captured := ChatRequest{
		Headers: r.Header.Clone(),
		Raw:     body,
	}
	if v, ok := raw["model"].(string); ok {
		captured.Model = v
	}
	if v, ok := raw["messages"].([]any); ok {
		for _, m := range v {
			if mm, ok := m.(map[string]any); ok {
				captured.Messages = append(captured.Messages, mm)
			}
		}
	}
	if v, ok := raw["tools"].([]any); ok {
		for _, t := range v {
			if tt, ok := t.(map[string]any); ok {
				captured.Tools = append(captured.Tools, tt)
			}
		}
	}
	m.requestsLock.Lock()
	m.requests = append(m.requests, captured)
	m.requestsLock.Unlock()

	if m.AuthChecker != nil {
		if status, body := m.AuthChecker(r.Header.Get("Authorization")); status != 0 {
			writeJSON(w, status, map[string]any{"error": map[string]string{"message": body}})
			return
		}
	}

	reply := m.popScripted()
	model := captured.Model
	if model == "" {
		model = "mock-default"
	}

	if reply.Status != 0 && reply.Status != http.StatusOK {
		writeJSON(w, reply.Status, map[string]any{
			"error": map[string]string{"type": "scripted_error", "message": reply.Content},
		})
		return
	}

	message := map[string]any{"role": "assistant"}
	finish := reply.FinishReason
	if len(reply.ToolCalls) > 0 {
		calls := make([]map[string]any, len(reply.ToolCalls))
		for i, tc := range reply.ToolCalls {
			calls[i] = map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Arguments,
				},
			}
		}
		message["tool_calls"] = calls
		if finish == "" {
			finish = "tool_calls"
		}
	} else {
		content := reply.Content
		if content == "" {
			content = m.Reply
		}
		message["content"] = content
		if finish == "" {
			finish = "stop"
		}
	}

	prompt := reply.PromptTokens
	if prompt == 0 {
		prompt = 1
	}
	completion := reply.CompletionTokens
	if completion == 0 {
		completion = 1
	}

	// Stream branch: when the request asked for stream:true (the
	// default path graph-llm + openai-go take), respond as a real
	// SSE stream of chat.completion.chunk events. Without this the
	// daemon's stream-delta envelopes never fire because the openai
	// SDK can't surface a stream response from a non-stream body.
	if streamFlag, _ := raw["stream"].(bool); streamFlag {
		writeChatStream(w, m.CallCount.Load(), model, message, finish, prompt, completion)
		return
	}

	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-mock-%d", m.CallCount.Load()),
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeChatStream emits the canonical OpenAI chat.completion stream:
// one chunk per token (split on whitespace so token boundaries are
// observable in conformance tests), one chunk per tool_call, and a
// final chunk carrying finish_reason + usage. The format mirrors the
// official streaming response so openai-go's NewStreaming parser is
// happy.
//
// We chose word-level token granularity (not char) on purpose: it
// gives streaming-aware tests something to assert on without making
// a single sentence produce hundreds of envelopes. The conformance
// test reassembles via "join with no separator" + restoring the
// space we strip on split — see TestE2E_Conformance_StreamDeltaPayload.
func writeChatStream(
	w http.ResponseWriter,
	callIdx int64,
	model string,
	message map[string]any,
	finish string,
	promptTokens, completionTokens int,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Test transport without flushing — fall back to the
		// non-stream JSON form so we at least don't 500.
		writeJSON(w, http.StatusOK, map[string]any{
			"id": fmt.Sprintf("chatcmpl-mock-%d", callIdx), "object": "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finish}},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id := fmt.Sprintf("chatcmpl-mock-%d", callIdx)

	emit := func(chunk map[string]any) {
		body, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(body)
		_, _ = w.Write([]byte("\n\n"))
		flusher.Flush()
	}

	// Role chunk first (matches OpenAI semantics: the first delta
	// carries {role: "assistant"} with no content).
	emit(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}}},
	})

	if calls, ok := message["tool_calls"].([]map[string]any); ok && len(calls) > 0 {
		// Tool-call branch: stream each call as one delta carrying
		// the full function definition. Real OpenAI splits these
		// across chunks (id+name first, arguments incrementally);
		// emitting them whole is sufficient for conformance and
		// keeps the mock simple.
		for i, tc := range calls {
			deltaCalls := []map[string]any{{
				"index":    i,
				"id":       tc["id"],
				"type":     "function",
				"function": tc["function"],
			}}
			emit(map[string]any{
				"id": id, "object": "chat.completion.chunk", "model": model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"tool_calls": deltaCalls}}},
			})
		}
	} else if content, _ := message["content"].(string); content != "" {
		// Content branch: split on whitespace and emit one chunk
		// per word, restoring the leading space on subsequent
		// words so reassembly is exactly the input string.
		parts := splitPreserveSpace(content)
		for _, p := range parts {
			emit(map[string]any{
				"id": id, "object": "chat.completion.chunk", "model": model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": p}}},
			})
		}
	}

	// Final chunk: empty delta + finish_reason + usage.
	emit(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
		"usage": map[string]int{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	})

	// OpenAI terminates the stream with a literal "[DONE]"
	// sentinel before EOF; openai-go relies on it to close the
	// stream cleanly.
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

// splitPreserveSpace splits s on single spaces, returning each word
// with its leading space (except the first). Reassembling via
// strings.Join("", parts) yields the exact input. Intentionally
// simple — we don't try to model OpenAI's actual tokenisation, just
// give consumers >1 chunk to assert ordering on.
func splitPreserveSpace(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i // include the space in the next chunk
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// popScripted returns the next scripted reply, or a zero value
// (which the caller treats as "use Reply field"). FIFO order,
// goroutine-safe.
func (m *MockOpenAI) popScripted() ScriptedReply {
	m.scriptedLock.Lock()
	defer m.scriptedLock.Unlock()
	if len(m.scripted) == 0 {
		return ScriptedReply{}
	}
	r := m.scripted[0]
	m.scripted = m.scripted[1:]
	return r
}

func readBody(r *http.Request) ([]byte, error) {
	const max = 1 << 20 // 1 MiB safety cap; tests never send anything close
	r.Body = http.MaxBytesReader(nil, r.Body, max)
	var buf strings.Builder
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				return []byte(buf.String()), nil
			}
			return []byte(buf.String()), err
		}
	}
}

// writeJSON is the small helper used by both handlers. We avoid
// importing the daemon's own writeJSON here to keep this helper
// dependency-free (only stdlib).
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	if err := enc.Encode(body); err != nil {
		_, _ = w.Write([]byte("\n"))
	}
}

// QuotedURL returns the URL wrapped in YAML-safe double quotes.
// Used by config-template helpers when the URL contains characters
// (like ':') that would otherwise need YAML escaping.
func (m *MockOpenAI) QuotedURL() string {
	return `"` + strings.ReplaceAll(m.URL(), `"`, `\"`) + `"`
}
