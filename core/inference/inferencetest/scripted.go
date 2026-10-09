package inferencetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ScriptedToolCall is one function call the scripted model returns.
type ScriptedToolCall struct {
	Name      string
	Arguments string
}

// ScriptedReply is one scripted assistant turn.
type ScriptedReply struct {
	Text      string
	ToolCalls []ScriptedToolCall
	// RequestID, when set, is echoed as the provider's x-request-id
	// response header so drivers can capture it (errors carry it on the
	// classified chain, streamed replies on the finish delta).
	RequestID string
	// ResponseID overrides the chat completion id echoed by the API
	// ("chatcmpl-fake" otherwise). Streamed replies mirror it on every
	// chunk.
	ResponseID string
	// Status, when non-zero, makes this reply an OpenAI-shaped HTTP
	// error instead of a completion. Error carries the API message.
	Status int
	Error  string
}

// ScriptedOpenAI is a scriptable, OpenAI-compatible chat completions
// endpoint speaking the real HTTP wire format (JSON and SSE streaming),
// so tests exercise the actual driver, retry, and stream-decoding paths
// instead of stubbing the Go runtime. It is the wire-level counterpart
// of the in-process fakes in this package: the suites check the shared
// Runtime contracts, the scripted server checks the wire.
//
// Replies are consumed in call order and the last one repeats for any
// further calls, so a script stays open-ended for retries. The server
// is stdlib-only, serves /v1/chat/completions, and shuts down with the
// test through [testing.TB.Cleanup]:
//
//	srv := inferencetest.NewScriptedOpenAI(t,
//		inferencetest.ScriptedReply{Text: "hello"},
//	)
//	// Point the driver under test at srv.URL() (the "/v1" base).
//	if got := srv.Calls(); got != 1 {
//		t.Fatalf("provider saw %d calls, want 1", got)
//	}
//	messages, err := srv.LastMessages()
type ScriptedOpenAI struct {
	*httptest.Server
	mu      sync.Mutex
	replies []ScriptedReply
	calls   int
	hold    *Gate
	bodies  [][]byte
	noUsage bool
}

// WithoutUsage makes every response omit its usage block, the way a
// provider that does not report tokens (or a proxy that strips them)
// does. Callers use it to exercise the paths that must survive a
// missing measurement instead of assuming one: the turnaround is
// zero-token usage, not an error.
func (s *ScriptedOpenAI) WithoutUsage() *ScriptedOpenAI {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noUsage = true
	return s
}

// reportsUsage tells the response writers whether to emit a usage block.
func (s *ScriptedOpenAI) reportsUsage() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.noUsage
}

// Gate pauses the next completion request until Release. It lets tests
// arrange deterministic ordering around an in-flight provider call (for
// example cutting a deadline or closing a runtime while a run is
// active).
type Gate struct {
	ready       chan struct{}
	release     chan struct{}
	readyOnce   sync.Once
	releaseOnce sync.Once
}

func newGate() *Gate {
	return &Gate{
		ready:   make(chan struct{}),
		release: make(chan struct{}),
	}
}

// Ready is closed once the gated request reaches the server.
func (g *Gate) Ready() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.ready
}

// Release unblocks the gated request.
func (g *Gate) Release() {
	if g == nil {
		return
	}
	g.releaseOnce.Do(func() { close(g.release) })
}

func (g *Gate) markReady() {
	if g == nil {
		return
	}
	g.readyOnce.Do(func() { close(g.ready) })
}

// HoldNext returns a gate applied to the next completion request. The
// gate is single-use, so a hold that no request consumes stays armed
// for whichever request comes next: a test that arms one for a turn a
// deadline can cut before its call goes out has to wait for Ready (or
// release it) before another turn can start, or that turn's call takes
// the hold and never gets an answer.
func (s *ScriptedOpenAI) HoldNext() *Gate {
	g := newGate()
	s.mu.Lock()
	s.hold = g
	s.mu.Unlock()
	return g
}

// NewScriptedOpenAI starts a scripted provider serving the given reply
// sequence. The last reply repeats for any further calls.
func NewScriptedOpenAI(t testing.TB, replies ...ScriptedReply) *ScriptedOpenAI {
	t.Helper()
	s := &ScriptedOpenAI{replies: replies}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// Calls returns the number of completion requests received.
func (s *ScriptedOpenAI) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// LastMessages decodes and returns the `messages` array of the most
// recent completion request, so tests can assert what actually reached
// the provider (system prompt, world sections, and the user turn).
func (s *ScriptedOpenAI) LastMessages() ([]map[string]any, error) {
	all, err := s.MessagesForCalls()
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[len(all)-1], nil
}

// MessagesForCalls decodes the `messages` array of every completion
// request received so far, oldest first. Several host-side generations
// share one provider (a turn, an auto-title, a post-turn review) and
// their completion order is not the test's to decide, so a test that
// wants to know what one of them sent reads the sequence instead of
// assuming which call came last.
func (s *ScriptedOpenAI) MessagesForCalls() ([][]map[string]any, error) {
	s.mu.Lock()
	bodies := make([][]byte, len(s.bodies))
	copy(bodies, s.bodies)
	s.mu.Unlock()
	out := make([][]map[string]any, 0, len(bodies))
	for _, body := range bodies {
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		out = append(out, req.Messages)
	}
	return out, nil
}

func (s *ScriptedOpenAI) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode request", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.calls++
	s.bodies = append(s.bodies, append([]byte(nil), body...))
	idx := s.calls - 1
	if idx >= len(s.replies) || len(s.replies) == 0 {
		idx = len(s.replies) - 1
	}
	reply := ScriptedReply{}
	if idx >= 0 {
		reply = s.replies[idx]
	}
	hold := s.hold
	s.hold = nil
	if hold != nil {
		hold.markReady()
	}
	s.mu.Unlock()
	if hold != nil {
		<-hold.release
	}

	if reply.RequestID != "" {
		w.Header().Set("x-request-id", reply.RequestID)
	}
	w.Header().Set("Content-Type", "application/json")
	if reply.Status != 0 {
		w.WriteHeader(reply.Status)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": reply.Error,
				"type":    "server_error",
				"code":    "server_error",
			},
		}); err != nil {
			return
		}
		return
	}
	if req.Stream {
		s.writeStream(w, reply, idx)
		return
	}
	if err := json.NewEncoder(w).Encode(s.completion(reply, idx)); err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
	}
}

// toolCallID mints one call id. Real providers issue a fresh id per
// call, so a script whose model repeats the same call still produces
// distinguishable ids; the request index keeps them unique per round
// even when the reply itself repeats.
func toolCallID(reqIdx, i int) string {
	return fmt.Sprintf("call_%d_%d", reqIdx, i+1)
}

func (s *ScriptedOpenAI) completion(reply ScriptedReply, reqIdx int) map[string]any {
	msg := map[string]any{"role": "assistant", "content": reply.Text}
	finish := "stop"
	responseID := reply.ResponseID
	if responseID == "" {
		responseID = "chatcmpl-fake"
	}
	if len(reply.ToolCalls) > 0 {
		msg["content"] = nil
		var calls []map[string]any
		for i, tc := range reply.ToolCalls {
			calls = append(calls, map[string]any{
				"id":   toolCallID(reqIdx, i),
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Arguments,
				},
			})
		}
		msg["tool_calls"] = calls
		finish = "tool_calls"
	}
	out := map[string]any{
		"id":      responseID,
		"object":  "chat.completion",
		"created": 1,
		"model":   "fake-model",
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if s.reportsUsage() {
		out["usage"] = map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
		}
	}
	return out
}

func (s *ScriptedOpenAI) writeStream(w http.ResponseWriter, reply ScriptedReply, reqIdx int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	responseID := reply.ResponseID
	if responseID == "" {
		responseID = "chatcmpl-fake"
	}
	var writeErr error
	chunk := func(delta map[string]any, finish any) {
		if writeErr != nil {
			return
		}
		payload := map[string]any{
			"id":      responseID,
			"object":  "chat.completion.chunk",
			"created": 1,
			"model":   "fake-model",
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		data, err := json.Marshal(payload)
		if err != nil {
			writeErr = err
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			writeErr = err
			return
		}
		flusher.Flush()
	}
	if len(reply.ToolCalls) > 0 {
		var calls []map[string]any
		for i, tc := range reply.ToolCalls {
			calls = append(calls, map[string]any{
				"id":   toolCallID(reqIdx, i),
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Arguments,
				},
			})
		}
		chunk(map[string]any{"role": "assistant", "tool_calls": calls}, nil)
		chunk(map[string]any{}, "tool_calls")
	} else {
		chunk(map[string]any{"role": "assistant"}, nil)
		if reply.Text != "" {
			chunk(map[string]any{"content": reply.Text}, nil)
		}
		chunk(map[string]any{}, "stop")
	}
	// Real providers report usage on their terminal chunk (OpenAI with
	// stream_options.include_usage). Without it the caller sees a
	// response whose token counts are all zero, which is how a streaming
	// driver that never reads usage would pass every test: the counts are
	// what the compaction anchor and the usage tables are built from.
	// Servers built with WithoutUsage drop it to keep that path covered
	// too.
	if s.reportsUsage() {
		writeUsageChunk(w, flusher, responseID)
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return
	}
	flusher.Flush()
}

// writeUsageChunk emits one usage-only chunk, the shape OpenAI sends
// when include_usage is set: an empty choices array plus a usage object.
func writeUsageChunk(w http.ResponseWriter, flusher http.Flusher, responseID string) {
	payload := map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": 1,
		"model":   "fake-model",
		"choices": []map[string]any{},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return
	}
	flusher.Flush()
}

// URL returns the base URL including the /v1 suffix the wire format
// expects.
func (s *ScriptedOpenAI) URL() string {
	return strings.TrimSuffix(s.Server.URL, "/") + "/v1"
}
