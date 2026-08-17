// Command provider is a minimal HTTP FlowCraft inference provider
// plugin. It serves the plugin RPC protocol v1 over JSON-RPC POST /
// and the streaming SSE endpoint /stream, answering generate with a
// canned echo response. It demonstrates the v1 anchor contract
// (inference.Provider/rpc) end to end.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
)

const protocolVersion = 1

var generateResponse = json.RawMessage(`{
  "message": {
    "role": "assistant",
    "content": {"parts": [{"type": "text", "text": "hello from the echo provider"}]}
  },
  "finish_reason": "completed",
  "usage": {"input_tokens": 1, "output_tokens": 2, "total_tokens": 3}
}`)

type request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type handshake struct {
	ProtocolVersion int          `json:"protocol_version"`
	Name            string       `json:"name"`
	Capabilities    []capability `json:"capabilities"`
}

type capability struct {
	Kind      string `json:"kind"`
	Impl      string `json:"impl"`
	Streaming bool   `json:"streaming,omitempty"`
}

type streamRequest struct {
	Handle string          `json:"handle"`
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args,omitempty"`
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()
	http.HandleFunc("/", handleRPC)
	http.HandleFunc("/stream", handleStream)
	log.Printf("echo provider listening on %s", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "echo provider:", err)
		os.Exit(1)
	}
}

func handleRPC(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		_ = json.NewEncoder(w).Encode(response{
			JSONRPC: "2.0", ID: -1,
			Error: &rpcError{Code: -32700, Message: "parse error"},
		})
		return
	}
	var result json.RawMessage
	var rpcErr *rpcError
	switch req.Method {
	case "plugin.handshake":
		result = mustJSON(handshake{
			ProtocolVersion: protocolVersion,
			Name:            "example.provider",
			Capabilities: []capability{{
				Kind:      "inference.Provider",
				Impl:      "echo",
				Streaming: true,
			}},
		})
	case "resource.new":
		result = mustJSON(struct {
			Handle string `json:"handle"`
		}{Handle: "provider-1"})
	case "resource.call":
		result = generateResponse
	case "resource.close":
		result = json.RawMessage("{}")
	default:
		rpcErr = &rpcError{Code: -32601, Message: "method not found"}
	}
	_ = json.NewEncoder(w).Encode(response{
		JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr,
	})
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	var req streamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid stream request", http.StatusBadRequest)
		return
	}
	if req.Handle == "" || req.Method != "generate_stream" {
		http.Error(w, "invalid stream request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	for _, event := range []string{
		`{"part_index":0,"delta":{"text":"hello "}}`,
		`{"part_index":0,"delta":{"text":"world"}}`,
		`{"finish_reason":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`,
	} {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		flusher.Flush()
	}
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
