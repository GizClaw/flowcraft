// Package mitm implements opt-in TLS termination for the sandbox
// enforcement proxy: a per-run temporary CA signs leaf certificates
// for CONNECT hosts, the proxy terminates TLS with the child, applies
// hooks, and re-establishes TLS to the real target. It is HTTP/1.1
// and HTTP/2 capable: ALPN offers both h2 and http/1.1, so gRPC-style
// h2-only clients are terminated too, with everything else falling
// back to h1. Host selection (MITMPolicy.Hosts / ExcludeHosts)
// decides which CONNECT tunnels are terminated at all.
package mitm

import (
	"context"
	"net/http"
)

// ConnectInfo describes one CONNECT attempt before any tunnel or TLS
// termination happens.
type ConnectInfo struct {
	Host string
	Port int
}

// RequestInfo is a decrypted request as seen by hooks. Body is only
// populated when body inspection is enabled and the request has a
// bounded Content-Length; streaming bodies pass through uninspected
// with Body nil.
type RequestInfo struct {
	Method string
	Scheme string
	Host   string
	Path   string
	Header http.Header
	Body   []byte
}

// ResponseInfo is a decrypted response as seen by hooks. Body follows
// the same bounded-Content-Length rule as RequestInfo.Body.
type ResponseInfo struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// ProxyHooks observes (and can block) decrypted traffic. Returning an
// error from any hook blocks the request/connection with 403.
// Hooks only run when MITM is enabled; plain CONNECT tunnels and
// plain HTTP forwarding never invoke them.
type ProxyHooks interface {
	OnConnect(ctx context.Context, c *ConnectInfo) error
	OnRequest(ctx context.Context, req *RequestInfo) error
	OnResponse(ctx context.Context, resp *ResponseInfo) error
}
