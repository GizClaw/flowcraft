package main

import (
	"bytes"
	"compress/gzip"
)

// The samples the MITM contract is dumped from. Each one is a scripted tunnel:
// what the client sends, what the fake upstream answers, and what the hooks are
// told. The harness in mitmcontract.go turns them into rows; nothing here knows
// how to run a socket.
//
// Authority is `example.test` everywhere except the row that exists to show it
// is not the SNI. Ports never appear: the client side is a listener the harness
// owns, and the dialer ignores the hostport it is handed.

// mitmGzipPlaintext is what the gzip rows compress. Both the upstream script and
// the ambient derivation read it, so the claim "the client got the plaintext" is
// checked against the bytes that went in rather than against a literal.
var mitmGzipPlaintext = []byte("hello hello hello")

// mitmGzipBody is `mitmGzipPlaintext` after one pass of Go's gzip writer. It is a
// constant of this file rather than something computed per run because the gate
// compares dumps byte for byte; `mtime` is zero and the OS byte is fixed, so the
// output is a function of the input alone.
var mitmGzipBody = func() []byte {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(mitmGzipPlaintext); err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
	return buffer.Bytes()
}()

// The request headers most rows send: the client names itself and refuses
// compression, so what the upstream sees is not a function of the client stack
// that happens to be running. The rows that exist to observe what the transport
// adds leave one or both of these out on purpose.
var mitmPlainHeaders = [][2]string{
	{"accept-encoding", "identity"},
	{"user-agent", "probe/1"},
}

func mitmGet(target string) mitmRequestSample {
	return mitmRequestSample{
		method:  "GET",
		target:  target,
		headers: mitmPlainHeaders,
		framing: mitmFramingNone,
	}
}

func mitmPost(target string, body []byte, framing string) mitmRequestSample {
	return mitmRequestSample{
		method:  "POST",
		target:  target,
		headers: mitmPlainHeaders,
		body:    body,
		framing: framing,
	}
}

// mitmAlpnSamples are the handshake rows: one client offer per row, plus one
// client that is not a client at all but the bytes of a TLS 1.1 hello.
var mitmAlpnSamples = []mitmAlpnSample{
	{
		name:    "both-preferred-by-the-client",
		offered: []string{"http/1.1", "h2"},
	},
	{
		name:    "both-preferred-by-us",
		offered: []string{"h2", "http/1.1"},
	},
	{
		name:    "h1-only",
		offered: []string{"http/1.1"},
	},
	{
		name:    "h2-only",
		offered: []string{"h2"},
	},
	{
		name:    "no-alpn",
		offered: nil,
	},
	{
		// A TLS 1.1 client is not expressible as a `crypto/tls` client on
		// this Go (1.0 and 1.1 are off by default), so the row is the
		// bytes such a client sends. That also makes it the one row both
		// stacks are handed identically -- see mitmTLS11Hello.
		name:  "tls-1-1-refused",
		hello: mitmTLS11Hello,
	},
	{
		name:       "tls-1-2-accepted",
		offered:    []string{"http/1.1"},
		maxVersion: "TLS 1.2",
	},
}

// mitmTLS11Hello is a ClientHello that says TLS 1.1 twice: in its legacy
// version and in the only entry of `supported_versions`. The legacy field alone
// would be read by a TLS 1.3 server as "1.2 or below" (RFC 8446 4.2.1), which
// makes the row about a missing extension rather than about the version floor;
// naming the version is what makes it unambiguous. The random is a constant so
// the dump stays reproducible -- nothing here is a security boundary.
func mitmTLS11HelloBytes() []byte {
	var hello []byte
	hello = append(hello, 0x03, 0x02) // legacy_version: TLS 1.1
	for index := 0; index < 32; index++ {
		hello = append(hello, byte(index))
	}
	hello = append(hello, 0x00)                   // session id length
	hello = append(hello, 0x00, 0x04)             // cipher suites length
	hello = append(hello, 0xc0, 0x2b, 0xc0, 0x2c) // ECDHE-ECDSA with AES-128/256-GCM
	hello = append(hello, 0x01, 0x00)             // compression: null
	// One extension: supported_versions = [TLS 1.1].
	extensions := []byte{0x00, 0x2b, 0x00, 0x03, 0x02, 0x03, 0x02}
	hello = append(hello, byte(len(extensions)>>8), byte(len(extensions)))
	hello = append(hello, extensions...)

	// The handshake header is a type byte and a three-byte length; getting
	// that length wrong makes the server wait for a body that never comes,
	// which looks exactly like a refusal and is not one.
	handshake := []byte{0x01, byte(len(hello) >> 16), byte(len(hello) >> 8), byte(len(hello))}
	handshake = append(handshake, hello...)

	record := []byte{0x16, 0x03, 0x02, byte(len(handshake) >> 8), byte(len(handshake))}
	return append(record, handshake...)
}

var mitmTLS11Hello = mitmTLS11HelloBytes()

// mitmSessionSamples is the tunnel corpus, in the order the rows appear. One row
// pins one thing; the numbering in the plan's list is this order.
var mitmSessionSamples = []mitmSessionSample{
	{
		name:       "get-allow-h1",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH1,
		requests:   []mitmRequestSample{mitmGet("/")},
		upstream:   mitmAnswerSample{status: 200, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
	},
	{
		name:       "get-allow-h2",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH2,
		requests:   []mitmRequestSample{mitmGet("/a?b=c")},
		upstream:   mitmAnswerSample{status: 200, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
	},
	{
		// Two requests on one decrypted connection: the trace is per request
		// and in arrival order, and the upstream answer script repeats.
		name:       "two-requests-one-tunnel",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH1,
		requests:   []mitmRequestSample{mitmGet("/first"), mitmGet("/second")},
		upstream:   mitmAnswerSample{status: 200, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
	},
	{
		name:       "no-alpn-still-h1",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoNoALPN,
		requests:   []mitmRequestSample{mitmGet("/")},
		upstream:   mitmAnswerSample{status: 200, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
	},
	{
		// The leaf has to carry an IP SAN: a client that verifies by address
		// refuses a certificate that only names it as a DNS string.
		name:       "an-ipv4-literal-verifies",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH1,
		serverName: "127.0.0.1",
		requests:   []mitmRequestSample{mitmGet("/")},
		upstream:   mitmAnswerSample{status: 200, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
	},
	{
		name:       "an-ipv6-literal-verifies",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH1,
		serverName: "::1",
		requests:   []mitmRequestSample{mitmGet("/")},
		upstream:   mitmAnswerSample{status: 200, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
	},
	{
		// The refusal is written by `http.Error`, so its shape -- 22 bytes,
		// `text/plain; charset=utf-8`, `nosniff` -- is core's answer and not
		// net/http's default for a handler that wrote nothing.
		name:         "deny-the-request",
		policy:       mitmPolicyRow{},
		negotiated:   mitmProtoH1,
		requests:     []mitmRequestSample{mitmPost("/submit", []byte("hello"), mitmFramingLength)},
		upstream:     mitmAnswerSample{status: 201, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
		requestHook:  mitmHookDeny,
		responseHook: mitmHookAllow,
	},
	{
		name:         "deny-the-response",
		policy:       mitmPolicyRow{},
		negotiated:   mitmProtoH1,
		requests:     []mitmRequestSample{mitmGet("/")},
		upstream:     mitmAnswerSample{status: 201, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
		requestHook:  mitmHookAllow,
		responseHook: mitmHookDeny,
	},
	{
		name:       "no-hooks",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH1,
		noHooks:    true,
		requests:   []mitmRequestSample{mitmGet("/")},
		upstream:   mitmAnswerSample{status: 200, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
	},
	{
		name:       "post-at-the-cap",
		policy:     mitmPolicyRow{InspectBodies: true, MaxBodyBytes: 5},
		negotiated: mitmProtoH1,
		requests:   []mitmRequestSample{mitmPost("/submit", []byte("hello"), mitmFramingLength)},
		upstream:   mitmAnswerSample{status: 201, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
	},
	{
		// One byte over: the hook is given no body, and the upstream still
		// receives all of it. Reading "a little, then deciding" truncates.
		name:       "post-over-the-cap",
		policy:     mitmPolicyRow{InspectBodies: true, MaxBodyBytes: 4},
		negotiated: mitmProtoH1,
		requests:   []mitmRequestSample{mitmPost("/submit", []byte("hello"), mitmFramingLength)},
		upstream:   mitmAnswerSample{status: 201, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
	},
	{
		// A chunked request has no declared length, so `ContentLength` is -1
		// and the body is never buffered however small it is. The upstream
		// sees it re-framed, which is the part a port gets wrong.
		name:       "post-chunked",
		policy:     mitmPolicyRow{InspectBodies: true, MaxBodyBytes: 4},
		negotiated: mitmProtoH1,
		requests:   []mitmRequestSample{mitmPost("/submit", []byte("hello"), mitmFramingChunked)},
		upstream:   mitmAnswerSample{status: 201, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingChunked},
	},
	{
		name:       "response-over-the-cap",
		policy:     mitmPolicyRow{InspectBodies: true, MaxBodyBytes: 4},
		negotiated: mitmProtoH1,
		requests:   []mitmRequestSample{mitmGet("/")},
		upstream:   mitmAnswerSample{status: 200, headers: mitmTextHeaders, body: []byte("hello"), framing: mitmFramingLength},
	},
	{
		name:       "response-chunked",
		policy:     mitmPolicyRow{InspectBodies: true, MaxBodyBytes: 4},
		negotiated: mitmProtoH1,
		requests:   []mitmRequestSample{mitmGet("/")},
		upstream:   mitmAnswerSample{status: 200, headers: mitmTextHeaders, body: []byte("hello"), framing: mitmFramingChunked},
	},
	{
		name:       "inspect-bodies-off",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH1,
		requests:   []mitmRequestSample{mitmPost("/submit", []byte("hello"), mitmFramingLength)},
		upstream:   mitmAnswerSample{status: 201, headers: mitmTextHeaders, body: []byte("hello"), framing: mitmFramingLength},
	},
	{
		name:       "upstream-refused",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH1,
		refuseDial: true,
		requests:   []mitmRequestSample{mitmGet("/")},
	},
	{
		name:       "upstream-tls-rejected",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH1,
		untrusted:  true,
		requests:   []mitmRequestSample{mitmGet("/")},
		upstream:   mitmAnswerSample{status: 200, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
	},
	{
		// 1 MiB of header is 1 byte more than `MaxHeaderBytes`, which is the
		// engine's number rather than net/http's default. Only the status is
		// the engine's answer: the head and body are net/http's.
		name:       "oversized-request-header",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH1,
		// 1 MiB is the engine's limit and net/http reads a few KiB past
		// it before giving up, so the pad has to clear the slack.
		padHeader:  1<<20 + 16<<10,
		requests:   []mitmRequestSample{mitmGet("/")},
		statusOnly: true,
	},
	{
		// The transport adds `accept-encoding: gzip` when the client did not,
		// and decompresses what it asked for. Both halves are visible from
		// outside, so both are pinned here -- the ambient section is derived
		// from this row.
		name:       "gzip-the-client-did-not-ask-for",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH1,
		requests: []mitmRequestSample{{
			method:  "GET",
			target:  "/gzip",
			framing: mitmFramingNone,
		}},
		upstream: mitmAnswerSample{
			status:  200,
			headers: mitmGzipHeaders,
			body:    mitmGzipBody,
			framing: mitmFramingLength,
		},
	},
	{
		// The other half of the ambient behavior, and core's blind spot: a
		// client that names its own encoding is served the compressed bytes
		// and the transport does not decompress, so the hook inspects gzip.
		name:       "gzip-the-client-asked",
		policy:     mitmPolicyRow{InspectBodies: true},
		negotiated: mitmProtoH1,
		requests: []mitmRequestSample{{
			method:  "GET",
			target:  "/gzip",
			headers: [][2]string{{"accept-encoding", "gzip"}, {"user-agent", "probe/1"}},
			framing: mitmFramingNone,
		}},
		upstream: mitmAnswerSample{
			status:  200,
			headers: mitmGzipHeaders,
			body:    mitmGzipBody,
			framing: mitmFramingLength,
		},
	},
	{
		// The authority the client sends is not the name the tunnel was
		// opened for. A port that rewrites the URL authority to the dial
		// address -- the obvious way to forward -- loses the Host header and
		// changes what the target sees.
		name:       "host-is-not-the-sni",
		policy:     mitmPolicyRow{},
		negotiated: mitmProtoH1,
		requests: []mitmRequestSample{{
			method:    "GET",
			target:    "/",
			authority: "other.test",
			headers:   mitmPlainHeaders,
			framing:   mitmFramingNone,
		}},
		upstream: mitmAnswerSample{status: 200, headers: mitmTextHeaders, body: []byte("ok"), framing: mitmFramingLength},
	},
}

var mitmTextHeaders = [][2]string{{"content-type", "text/plain"}}

// Only the rows that exist to observe gzip declare one: every other row's
// answer is a body a client can read without decoding it.
var mitmGzipHeaders = [][2]string{
	{"content-encoding", "gzip"},
	{"content-type", "text/plain"},
}
