package a2a_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/agenttest"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/agent/a2a"
	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
)

// rpcErr is a scripted JSON-RPC error response.
type rpcErr struct {
	code int
	msg  string
}

// fakeA2A is a scriptable A2A JSON-RPC server for tests. Handlers default to
// a 500 if left nil; tests set the ones they exercise.
type fakeA2A struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	sendFn   func(params json.RawMessage) (any, *rpcErr)
	getFn    func(params json.RawMessage) (any, *rpcErr)
	cancelFn func(params json.RawMessage) (any, *rpcErr)
	// streamFn returns the SSE data payloads (complete JSON-RPC responses)
	// to emit, in order.
	streamFn func(params json.RawMessage) []json.RawMessage

	sends    int
	gets     int
	cancels  int
	streams  int
	lastSend json.RawMessage
}

func newFakeA2A(t *testing.T) *fakeA2A {
	t.Helper()
	f := &fakeA2A{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeA2A) url() string { return f.srv.URL }

func (f *fakeA2A) serveHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// A2A v1.0 renamed JSON-RPC methods to CamelCase; the 0.3 compat
	// transport keeps the slash-separated names. Normalise both.
	var kind string
	switch req.Method {
	case "SendStreamingMessage", "message/stream", "SubscribeToTask", "tasks/resubscribe":
		kind = "stream"
	case "SendMessage", "message/send":
		kind = "send"
	case "GetTask", "tasks/get":
		kind = "get"
	case "CancelTask", "tasks/cancel":
		kind = "cancel"
	}
	switch kind {
	case "stream":
		f.mu.Lock()
		f.streams++
		fn := f.streamFn
		f.mu.Unlock()
		if fn == nil {
			http.Error(w, "no stream handler", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, data := range fn(req.Params) {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		return
	case "send":
		f.mu.Lock()
		f.sends++
		fn := f.sendFn
		f.lastSend = append(json.RawMessage(nil), req.Params...)
		f.mu.Unlock()
		if fn == nil {
			http.Error(w, "no send handler", http.StatusInternalServerError)
			return
		}
		result, rpcErr := fn(req.Params)
		f.writeResult(w, req.ID, result, rpcErr)
	case "get":
		f.mu.Lock()
		f.gets++
		fn := f.getFn
		f.mu.Unlock()
		if fn == nil {
			http.Error(w, "no get handler", http.StatusInternalServerError)
			return
		}
		result, rpcErr := fn(req.Params)
		f.writeResult(w, req.ID, result, rpcErr)
	case "cancel":
		f.mu.Lock()
		f.cancels++
		fn := f.cancelFn
		f.mu.Unlock()
		if fn == nil {
			http.Error(w, "no cancel handler", http.StatusInternalServerError)
			return
		}
		result, rpcErr := fn(req.Params)
		f.writeResult(w, req.ID, result, rpcErr)
	default:
		http.Error(w, "unknown method "+req.Method, http.StatusNotFound)
	}
}

func (f *fakeA2A) writeResult(w http.ResponseWriter, id any, result any, rpcErr *rpcErr) {
	w.Header().Set("Content-Type", "application/json")
	if rpcErr != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": rpcErr.code, "message": rpcErr.msg},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *fakeA2A) counts() (sends, gets, cancels, streams int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sends, f.gets, f.cancels, f.streams
}

// ---------- wire builders ----------

// taskV1 builds a v1.0 (or 0.3) wire Task object.
func taskV1(id, ctxID, state string, history, artifacts []any) map[string]any {
	out := map[string]any{
		"id":        id,
		"contextId": ctxID,
		"status":    map[string]any{"state": state},
	}
	if history != nil {
		out["history"] = history
	}
	if artifacts != nil {
		out["artifacts"] = artifacts
	}
	return out
}

// msgV1 builds a v1.0 wire Message object.
func msgV1(id, role string, parts []any) map[string]any {
	return map[string]any{"messageId": id, "role": role, "parts": parts}
}

// card builds an AgentCard pointing at url with the given streaming
// capability and protocol version.
func card(url string, streaming bool, protocolVersion a2aprotocol.ProtocolVersion) *a2aprotocol.AgentCard {
	return &a2aprotocol.AgentCard{
		Name:               "fake-remote",
		Description:        "fake remote agent for tests",
		Version:            "0.0.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []a2aprotocol.AgentSkill{{ID: "s", Name: "s"}},
		SupportedInterfaces: []*a2aprotocol.AgentInterface{{
			URL:             url,
			ProtocolBinding: a2aprotocol.TransportProtocolJSONRPC,
			ProtocolVersion: protocolVersion,
		}},
		Capabilities: a2aprotocol.AgentCapabilities{Streaming: streaming},
	}
}

// newTestEngine builds an engine against the fake server with a plain HTTP
// client (no retries) for deterministic tests.
func newTestEngine(t *testing.T, f *fakeA2A, streaming bool, opts ...a2a.Option) *a2a.Engine {
	t.Helper()
	allOpts := append([]a2a.Option{a2a.WithHTTPClient(&http.Client{})}, opts...)
	eng, err := a2a.New(context.Background(), card(f.url(), streaming, a2aprotocol.Version), allOpts...)
	if err != nil {
		t.Fatalf("a2a.New: %v", err)
	}
	return eng
}

// runTurn drives one full agent.Execute against eng and returns the result.
func runTurn(t *testing.T, eng *a2a.Engine, userText string, opts ...agent.ExecuteOption) *agent.Result {
	t.Helper()
	host := agenttest.NewMockHost()
	opts = append([]agent.ExecuteOption{agent.WithHost(host)}, opts...)
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "test-agent"}, eng, agent.Request{
		Message: message.Message{
			Role:    message.RoleUser,
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: userText}}},
		},
	}, opts...)
	if err != nil {
		t.Fatalf("agent.Execute: %v", err)
	}
	return res
}
