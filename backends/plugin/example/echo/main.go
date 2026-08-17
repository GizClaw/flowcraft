// Command echo is a minimal FlowCraft stdio plugin. It implements the
// plugin RPC protocol v1 as a newline-delimited JSON server on
// stdin/stdout: handshake, resource.new, resource.call (echoes the
// args back) and resource.close. It is the Go-side reference for the
// service slot.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const protocolVersion = 1

func main() {
	if err := serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "echo plugin:", err)
		os.Exit(1)
	}
}

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
	Kind string `json:"kind"`
	Impl string `json:"impl"`
	Spec spec   `json:"spec"`
}

type spec struct {
	Deps     []any  `json:"deps,omitempty"`
	ItemType string `json:"item_type,omitempty"`
}

func serve(stdin io.Reader, stdout io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	encoder := json.NewEncoder(stdout)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = encoder.Encode(response{
				JSONRPC: "2.0", ID: -1,
				Error: &rpcError{Code: -32700, Message: "parse error"},
			})
			continue
		}
		result, rpcErr := dispatch(req)
		_ = encoder.Encode(response{
			JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr,
		})
	}
	return scanner.Err()
}

func dispatch(req request) (json.RawMessage, *rpcError) {
	switch req.Method {
	case "plugin.handshake":
		return mustJSON(handshake{
			ProtocolVersion: protocolVersion,
			Name:            "example.echo",
			Capabilities: []capability{{
				Kind: "example.Echo",
				Impl: "echo",
				Spec: spec{Deps: []any{}},
			}},
		}), nil
	case "resource.new":
		return mustJSON(struct {
			Handle string `json:"handle"`
		}{Handle: "echo-1"}), nil
	case "resource.call":
		var params struct {
			Args json.RawMessage `json:"args"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if len(params.Args) == 0 {
			params.Args = json.RawMessage("{}")
		}
		return params.Args, nil
	case "resource.close":
		return json.RawMessage("{}"), nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
