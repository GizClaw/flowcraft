package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// request is the JSON-RPC 2.0 request envelope written by the host.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// response is the JSON-RPC 2.0 response envelope read from the plugin.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object returned by the plugin.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "nil plugin rpc error"
	}
	if e.Message != "" {
		return fmt.Sprintf("plugin rpc error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("plugin rpc error %d", e.Code)
}

type handshakeParams struct {
	ProtocolVersions []int  `json:"protocol_versions"`
	HostName         string `json:"host_name"`
	HostCoreVersion  string `json:"host_core_version"`
}

type newParams struct {
	Capability string          `json:"capability"`
	Settings   json.RawMessage `json:"settings,omitempty"`
}

type callParams struct {
	Handle string          `json:"handle"`
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args,omitempty"`
}

type closeParams struct {
	Handle string `json:"handle"`
}

// readLine reads one newline-terminated JSON line from r, returning
// the bytes without the newline. It aborts when ctx ends, in which
// case the blocking reader is abandoned.
func readLine(ctx context.Context, r *bufio.Reader, max int64) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := readLineBlocking(r, max)
		ch <- result{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		return res.line, res.err
	}
}

func readLineBlocking(r *bufio.Reader, max int64) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if int64(len(buf)) > max {
			return nil, fmt.Errorf(
				"response exceeds payload limit of %d bytes", max)
		}
		switch {
		case err == nil:
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(buf) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		default:
			return nil, err
		}
	}
}
