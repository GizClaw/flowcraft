package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"

	corenet "github.com/GizClaw/flowcraft/core/utils/net"
	"github.com/GizClaw/flowcraft/core/utils/net/mitm"
)

// The MITM contract: what a terminated tunnel is, once the TLS is off.
//
// The rest of core/utils/net is pinned elsewhere. Which hosts are terminated is
// a pure function and lives in proxy-contract.json; what a `CONNECT` is answered
// is a socket and lives in wire-contract.json. What is left is the layer under
// the termination: the handshake the engine offers, and the decrypted traffic --
// what the hooks see, what the client gets back, and what the target receives.
//
//	alpn     -- one handshake per row: what the client offered, what was
//	            negotiated, and the protocol version. Plus one row whose client
//	            is the bytes of a TLS 1.1 hello, because neither stack can be
//	            asked for one.
//	ambient  -- what the forwarding transport adds to a request nobody dressed
//	            up, derived from the one session row that leaves the headers off.
//	sessions -- one tunnel per row: the hooks' view, the client's answer, and the
//	            request the fake upstream received.
//
// Four normalizations make rows comparable across two HTTP stacks, and each one
// is the difference between a contract and a transcript:
//
//   - Header names are lowercased and sorted; repeated values keep the order
//     they arrived in. The authority is not a header here: core's server moves
//     `Host` out of the map for h1 and `:authority` for h2, so it is a field.
//   - A body is hex. `null` is "the hook was not given a body" -- inspection
//     off, length unknown, or over the cap -- and `""` is a body that was given
//     and was empty. That difference is the only evidence that a body was
//     inspected at all.
//   - `date` and `connection` are masked: the first is a clock, the second is
//     the writer's framing decision. Everything else a reader can act on stays.
//   - One request is in flight at a time, so trace order is arrival order.
//
// The certificates themselves are not rows. Their real contract -- the chain,
// the names, the SAN type, the usage, the validity -- is verified by every
// session row, because a client that trusts the root and completes a handshake
// has checked all of it. The parts no client looks at are pinned in ca's own
// tests; the one branch worth a wire is the SAN type, which is what the two
// literal rows are.
const mitmContractNote = "core/utils/net/mitm: the layer under the terminated tunnel -- the " +
	"handshake the engine offers, what hooks see, what the client gets back, and what the target " +
	"receives. Header names are lowercased and sorted and the authority is a field, not a header, " +
	"because core's server moves it out of the map differently per protocol. Bodies are hex; null " +
	"means the hook was given no body, \"\" means an empty one. `date` and `connection` are masked. " +
	"A row's requests are sent in order on one connection, and its upstream answer is the answer " +
	"to each of them. Ports never appear: the client side is a listener the harness owns and the " +
	"dialer ignores the address it is handed. `server_name` is present only when the tunnel is not " +
	"opened for example.test, `untrusted` marks an upstream certificate no root holds, and " +
	"`refuse_dial` a dial that fails instead of connecting."

const (
	// mitmProtoH1, mitmProtoH2 and mitmProtoNoALPN are what the client offers
	// -- and so also what the handshake negotiates. The engine offers both
	// protocols; a client that offers neither gets h1.
	mitmProtoH1     = "http/1.1"
	mitmProtoH2     = "h2"
	mitmProtoNoALPN = ""

	mitmFramingNone    = "none"
	mitmFramingLength  = "length"
	mitmFramingChunked = "chunked"

	mitmHookAllow = "allow"
	mitmHookDeny  = "deny"

	// mitmServerName is the host every row's tunnel is opened for, and the
	// name the fake upstream's certificate covers.
	mitmServerName = "example.test"

	// mitmCap is what the harness keeps of a body it did not mean to bound.
	mitmCap = 1 << 21

	mitmHandshakeTimeout = 10 * time.Second
	mitmSessionTimeout   = 20 * time.Second
)

// mitmContract is the document. The tables are separate because they are read
// by separate replays: the handshake ones need no engine.
type mitmContract struct {
	Note     string           `json:"note"`
	ALPN     []mitmAlpnRow    `json:"alpn"`
	Ambient  mitmAmbientRow   `json:"ambient"`
	Sessions []mitmSessionRow `json:"sessions"`
}

// mitmAlpnRow is one handshake. `offered` is the ALPN list a real client sends;
// `hello` is the bytes of a handshake a real client cannot be asked for. Exactly
// one of them is set.
type mitmAlpnRow struct {
	Name       string   `json:"name"`
	Offered    []string `json:"offered,omitempty"`
	Hello      string   `json:"hello,omitempty"`
	MaxVersion string   `json:"max_version,omitempty"`
	OK         bool     `json:"ok"`
	Negotiated string   `json:"negotiated,omitempty"`
	TLSVersion string   `json:"tls_version,omitempty"`
}

// mitmAmbientRow is what the outbound transport does to a request on its own.
// It is derived from the session row named in `row`, so it cannot drift from it.
type mitmAmbientRow struct {
	Row                 string            `json:"row"`
	AddsWhenAbsent      map[string]string `json:"adds_when_absent"`
	DecompressesOwnGzip bool              `json:"decompresses_own_gzip"`
	UpstreamProto       string            `json:"upstream_proto"`
}

// mitmSessionRow is one tunnel.
//
// Three fields are there because a replay cannot infer them and would silently
// replay a different tunnel: `server_name` when the tunnel is not opened for
// mitmServerName, `untrusted` when the upstream presents a certificate no root
// holds, and `refuse_dial` when there is no upstream at all. A client answer
// without a `framing` is the one row whose head and body are net/http's rather
// than the engine's, so only its status is compared.
type mitmSessionRow struct {
	Name        string           `json:"name"`
	Policy      mitmPolicyRow    `json:"policy"`
	Negotiated  string           `json:"negotiated"`
	ServerName  string           `json:"server_name,omitempty"`
	Untrusted   bool             `json:"untrusted,omitempty"`
	RefuseDial  bool             `json:"refuse_dial,omitempty"`
	Requests    []mitmRequestRow `json:"requests"`
	Upstream    *mitmAnswerRow   `json:"upstream,omitempty"`
	Hooks       *mitmHooksRow    `json:"hooks,omitempty"`
	Trace       []mitmTraceEvent `json:"trace"`
	Client      []mitmAnswerRow  `json:"client"`
	UpstreamSaw []mitmSeenRow    `json:"upstream_saw"`
}

// mitmPolicyRow is the part of the MITM policy the engine reads.
type mitmPolicyRow struct {
	InspectBodies bool  `json:"inspect_bodies"`
	MaxBodyBytes  int64 `json:"max_body_bytes,omitempty"`
}

// mitmHooksRow is which hooks were installed and what they decided.
type mitmHooksRow struct {
	Request  string `json:"request"`
	Response string `json:"response"`
}

// mitmRequestRow is one request the client sends. `framing` is how it is
// declared on the wire: a `Content-Length`, a chunked body, or no body at all.
type mitmRequestRow struct {
	Method    string              `json:"method"`
	Target    string              `json:"target"`
	Authority string              `json:"authority"`
	Headers   map[string][]string `json:"headers,omitempty"`
	Body      string              `json:"body,omitempty"`
	Framing   string              `json:"framing"`
	// PadHeader makes the client send an `x-pad` header of that many bytes,
	// which is how the header-size limit is reached without a megabyte of
	// committed JSON.
	PadHeader int `json:"pad_header,omitempty"`
}

// mitmAnswerRow is a response: the upstream's script, or the client's answer.
// `Body` is a pointer so that "not compared" and "empty" are different rows.
type mitmAnswerRow struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    *string             `json:"body,omitempty"`
	Framing string              `json:"framing,omitempty"`
}

// mitmTraceEvent is one hook call.
type mitmTraceEvent struct {
	Event   string              `json:"event"`
	Method  string              `json:"method,omitempty"`
	Scheme  string              `json:"scheme,omitempty"`
	Host    string              `json:"host,omitempty"`
	Path    string              `json:"path,omitempty"`
	Status  int                 `json:"status,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    *string             `json:"body"`
}

// mitmSeenRow is one request the fake upstream received.
type mitmSeenRow struct {
	Method    string              `json:"method"`
	Target    string              `json:"target"`
	Authority string              `json:"authority"`
	SNI       string              `json:"sni,omitempty"`
	Proto     string              `json:"proto"`
	Headers   map[string][]string `json:"headers,omitempty"`
	Body      *string             `json:"body,omitempty"`
	Framing   string              `json:"framing"`
}

// mitmAlpnSample is one handshake to run.
type mitmAlpnSample struct {
	name       string
	offered    []string
	hello      []byte
	maxVersion string
}

// mitmSessionSample is one scripted tunnel. Everything a row needs to be
// reproduced is in here, and nothing here knows how to open a socket.
type mitmSessionSample struct {
	name       string
	policy     mitmPolicyRow
	negotiated string
	// serverName is the name the tunnel is opened for; empty means
	// mitmServerName.
	serverName string
	requests   []mitmRequestSample
	upstream   mitmAnswerSample
	// requestHook and responseHook decide; empty means allow.
	requestHook  string
	responseHook string
	// noHooks installs none, which is the one case with an empty trace.
	noHooks bool
	// untrusted has the upstream present a certificate no root store holds.
	untrusted bool
	// refuseDial fails the dial instead of connecting.
	refuseDial bool
	// padHeader is how much `x-pad` the client sends.
	padHeader int
	// statusOnly keeps the status and drops the head, for a row whose status
	// line is the engine's answer and whose head is net/http's.
	statusOnly bool
}

// mitmRequestSample is one request in a script.
type mitmRequestSample struct {
	method    string
	target    string
	authority string
	headers   [][2]string
	body      []byte
	framing   string
}

// mitmAnswerSample is what the fake upstream answers to each request.
type mitmAnswerSample struct {
	status  int
	headers [][2]string
	body    []byte
	framing string
}

func writeMITMContract(w io.Writer) error {
	contract := mitmContract{Note: mitmContractNote}
	alpn, err := mitmAlpnRows()
	if err != nil {
		return fmt.Errorf("alpn: %w", err)
	}
	contract.ALPN = alpn
	for _, sample := range mitmSessionSamples {
		row, err := mitmSessionRowOf(sample)
		if err != nil {
			return fmt.Errorf("session %s: %w", sample.name, err)
		}
		contract.Sessions = append(contract.Sessions, row)
	}
	ambient, err := mitmAmbientOf(contract.Sessions)
	if err != nil {
		return fmt.Errorf("ambient: %w", err)
	}
	contract.Ambient = ambient
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(contract)
}

// ------------------------------------------------------------------ handshakes

// mitmAlpnRows runs every handshake sample against a real `tls.Server` built
// the way the engine builds one: h2 preferred, http/1.1 next, TLS 1.2 at the
// bottom.
func mitmAlpnRows() ([]mitmAlpnRow, error) {
	ca, err := mitm.NewCA()
	if err != nil {
		return nil, err
	}
	leaf, err := ca.Leaf(mitmServerName)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.PEM()) {
		return nil, errors.New("the CA's PEM does not load")
	}
	var rows []mitmAlpnRow
	for _, sample := range mitmAlpnSamples {
		row, err := mitmAlpnRowOf(sample, *leaf, roots)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sample.name, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func mitmAlpnRowOf(sample mitmAlpnSample, leaf tls.Certificate, roots *x509.CertPool) (mitmAlpnRow, error) {
	row := mitmAlpnRow{
		Name:       sample.name,
		Offered:    sample.offered,
		Hello:      hexOrEmpty(sample.hello),
		MaxVersion: sample.maxVersion,
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return row, err
	}
	defer func() { _ = listener.Close() }()

	// The row's verdict is the server's: a client that gives up before the
	// server does is still a refused handshake, and for the byte rows there
	// is no client to ask.
	type served struct {
		negotiated string
		version    string
		err        error
	}
	done := make(chan served, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- served{err: err}
			return
		}
		server := tls.Server(conn, &tls.Config{
			Certificates: []tls.Certificate{leaf},
			NextProtos:   []string{mitmProtoH2, mitmProtoH1},
			MinVersion:   tls.VersionTLS12,
		})
		defer func() { _ = server.Close() }()
		if err := server.Handshake(); err != nil {
			done <- served{err: err}
			return
		}
		state := server.ConnectionState()
		done <- served{negotiated: state.NegotiatedProtocol, version: tls.VersionName(state.Version)}
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		return row, err
	}
	_ = conn.SetDeadline(time.Now().Add(mitmHandshakeTimeout))
	if sample.hello != nil {
		// Read whatever the server says back before hanging up: closing
		// straight after the write can reach the peer as a reset, and then
		// the server never sees the hello at all.
		if _, err := conn.Write(sample.hello); err != nil {
			_ = conn.Close()
			return row, fmt.Errorf("write hello: %w", err)
		}
		_, _ = io.Copy(io.Discard, conn)
	} else {
		config := &tls.Config{
			RootCAs:    roots,
			ServerName: mitmServerName,
			NextProtos: sample.offered,
			MinVersion: tls.VersionTLS12,
		}
		if sample.maxVersion != "" {
			version, err := mitmVersion(sample.maxVersion)
			if err != nil {
				_ = conn.Close()
				return row, err
			}
			config.MaxVersion = version
		}
		client := tls.Client(conn, config)
		// A failure here is the row's answer, not the harness's: the row
		// records what the server said, which is the refused handshake.
		_ = client.Handshake()
		_ = client.Close()
	}
	_ = conn.Close()

	select {
	case result := <-done:
		if result.err != nil {
			return row, nil
		}
		row.OK = true
		row.Negotiated = result.negotiated
		row.TLSVersion = result.version
		return row, nil
	case <-time.After(mitmHandshakeTimeout):
		return row, errors.New("the handshake neither finished nor failed")
	}
}

// mitmVersion maps the version names a row carries to the constant a client is
// configured with.
func mitmVersion(name string) (uint16, error) {
	switch name {
	case "TLS 1.2":
		return tls.VersionTLS12, nil
	case "TLS 1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("no client can be pinned to %q", name)
	}
}

// -------------------------------------------------------------------- ambient

// mitmAmbientOf reads the transport's own behavior off the session row that
// leaves the headers off. Every field is checked against that row rather than
// written down, so a change in core moves the ambient section with it.
func mitmAmbientOf(rows []mitmSessionRow) (mitmAmbientRow, error) {
	const name = "gzip-the-client-did-not-ask-for"
	ambient := mitmAmbientRow{Row: name, AddsWhenAbsent: map[string]string{}}
	var row *mitmSessionRow
	for index := range rows {
		if rows[index].Name == name {
			row = &rows[index]
			break
		}
	}
	if row == nil {
		return ambient, fmt.Errorf("the row the ambient section is derived from is gone: %s", name)
	}
	if len(row.UpstreamSaw) != 1 || len(row.Client) != 1 {
		return ambient, fmt.Errorf("%s: expected one request and one answer, got %d and %d",
			name, len(row.UpstreamSaw), len(row.Client))
	}
	seen, answer := row.UpstreamSaw[0], row.Client[0]
	for _, header := range []string{"accept-encoding", "user-agent"} {
		values := seen.Headers[header]
		if len(values) == 0 {
			return ambient, fmt.Errorf("%s: the request the upstream received has no %s", name, header)
		}
		ambient.AddsWhenAbsent[header] = values[0]
	}
	plaintext := hexOf(mitmGzipPlaintext)
	if answer.Body == nil || *answer.Body != plaintext {
		return ambient, fmt.Errorf("%s: the client was not given the plaintext", name)
	}
	if _, encoded := answer.Headers["content-encoding"]; encoded {
		return ambient, fmt.Errorf("%s: the client's answer still declares an encoding", name)
	}
	ambient.DecompressesOwnGzip = true
	ambient.UpstreamProto = seen.Proto
	return ambient, nil
}

// -------------------------------------------------------------------- sessions

func mitmSessionRowOf(sample mitmSessionSample) (row mitmSessionRow, err error) {
	row = mitmSessionRow{
		Name:       sample.name,
		Policy:     sample.policy,
		Negotiated: sample.negotiated,
		Untrusted:  sample.untrusted,
		RefuseDial: sample.refuseDial,
		Requests:   mitmRequestRows(sample.requests),
	}
	if sample.serverName != "" && sample.serverName != mitmServerName {
		row.ServerName = sample.serverName
	}
	// The pad is a property of the script, and the replay needs it to send the
	// same head: a row that only recorded the limit would replay a small request
	// and see it served.
	if sample.padHeader > 0 {
		row.Requests[0].PadHeader = sample.padHeader
	}
	if !sample.noHooks {
		row.Hooks = &mitmHooksRow{
			Request:  mitmHookName(sample.requestHook),
			Response: mitmHookName(sample.responseHook),
		}
	}
	if len(sample.requests) == 0 {
		return row, errors.New("no request to send")
	}
	serverName := sample.serverName
	if serverName == "" {
		serverName = mitmServerName
	}

	trusted, err := mitm.NewCA()
	if err != nil {
		return row, err
	}
	if sample.upstream.status != 0 {
		answer := sample.upstream.row()
		row.Upstream = &answer
	}
	var stub *mitmStub
	if !sample.refuseDial {
		// The upstream's certificate comes from a CA of its own, which the
		// engine is handed as its roots. The rejected row presents one from
		// a second CA instead, and the roots stay what they were.
		issuer := trusted
		if sample.untrusted {
			if issuer, err = mitm.NewCA(); err != nil {
				return row, err
			}
		}
		leaf, err := issuer.Leaf(serverName)
		if err != nil {
			return row, err
		}
		if stub, err = startMITMStub(sample.upstream, *leaf); err != nil {
			return row, err
		}
		defer stub.stop()
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(trusted.PEM()) {
		return row, errors.New("the upstream CA's PEM does not load")
	}

	dialer := &net.Dialer{Timeout: mitmHandshakeTimeout}
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		if sample.refuseDial {
			return nil, errors.New("the harness refuses the dial")
		}
		return dialer.DialContext(ctx, "tcp", stub.address())
	}
	var hooks corenet.MITMHooks
	var recorder *mitmRecorder
	if !sample.noHooks {
		recorder = &mitmRecorder{
			denyRequest:  sample.requestHook == mitmHookDeny,
			denyResponse: sample.responseHook == mitmHookDeny,
		}
		hooks = recorder
	}
	engine, err := mitm.New(&corenet.MITMPolicy{
		Enabled:       true,
		InspectBodies: sample.policy.InspectBodies,
		MaxBodyBytes:  sample.policy.MaxBodyBytes,
	}, hooks, dial, roots)
	if err != nil {
		return row, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return row, err
	}
	served := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		served <- engine.Serve(conn, serverName, serverName+":443")
	}()
	// The listener stays open until the session is over: a connection that
	// is still sitting in its backlog is dropped by Close, and the engine
	// would be handed nothing at all.
	defer func() { _ = listener.Close() }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		return row, err
	}
	defer func() { _ = conn.Close() }()

	clientRoots := x509.NewCertPool()
	if !clientRoots.AppendCertsFromPEM(engine.PEM()) {
		return row, errors.New("the engine's PEM does not load")
	}
	client := tls.Client(conn, &tls.Config{
		RootCAs:    clientRoots,
		ServerName: serverName,
		NextProtos: mitmALPN(sample.negotiated),
		MinVersion: tls.VersionTLS12,
	})
	handshake, cancel := context.WithTimeout(context.Background(), mitmHandshakeTimeout)
	defer cancel()
	if err := client.HandshakeContext(handshake); err != nil {
		select {
		case serveErr := <-served:
			return row, fmt.Errorf("client handshake: %w (engine: %v)", err, serveErr)
		case <-time.After(time.Second):
		}
		return row, fmt.Errorf("client handshake: %w", err)
	}
	negotiated := client.ConnectionState().NegotiatedProtocol
	if negotiated != sample.negotiated {
		return row, fmt.Errorf("the client offered %q and got %q", sample.negotiated, negotiated)
	}

	client.SetDeadline(time.Now().Add(mitmSessionTimeout))
	if sample.negotiated == mitmProtoH2 {
		answers, err := mitmH2Exchange(client, sample)
		if err != nil {
			return row, err
		}
		row.Client = answers
	} else {
		reader := bufio.NewReader(client)
		for _, request := range sample.requests {
			answer, err := mitmH1Exchange(client, reader, request, sample.padHeader)
			if err != nil {
				return row, fmt.Errorf("%s %s: %w", request.method, request.target, err)
			}
			if sample.statusOnly {
				answer = mitmAnswerRow{Status: answer.Status}
			}
			row.Client = append(row.Client, answer)
		}
	}

	if recorder != nil {
		row.Trace = recorder.snapshot()
	}
	if row.Trace == nil {
		row.Trace = []mitmTraceEvent{}
	}
	if stub != nil {
		row.UpstreamSaw = stub.snapshot()
	}
	if row.UpstreamSaw == nil {
		row.UpstreamSaw = []mitmSeenRow{}
	}
	_ = client.Close()
	select {
	case err := <-served:
		if err != nil {
			return row, fmt.Errorf("serve: %w", err)
		}
	case <-time.After(mitmSessionTimeout):
		return row, errors.New("the engine did not return after the client closed")
	}
	return row, nil
}

func mitmALPN(negotiated string) []string {
	if negotiated == mitmProtoNoALPN {
		return nil
	}
	return []string{negotiated}
}

// mitmH1Exchange writes one request as bytes and reads the answer back. The
// wire is written out rather than handed to a client library because the
// framing is part of the script: a chunked request and one that declares its
// length are the two cases the cap distinguishes.
func mitmH1Exchange(conn net.Conn, reader *bufio.Reader, request mitmRequestSample, padHeader int) (mitmAnswerRow, error) {
	var buffer bytes.Buffer
	authority := request.authority
	if authority == "" {
		authority = mitmServerName
	}
	fmt.Fprintf(&buffer, "%s %s HTTP/1.1\r\nHost: %s\r\n", request.method, request.target, authority)
	for _, header := range mitmSortedHeaders(mitmHeaderMap(request.headers)) {
		fmt.Fprintf(&buffer, "%s: %s\r\n", header[0], header[1])
	}
	if padHeader > 0 {
		fmt.Fprintf(&buffer, "x-pad: %s\r\n", strings.Repeat("x", padHeader))
	}
	switch request.framing {
	case mitmFramingLength:
		fmt.Fprintf(&buffer, "Content-Length: %d\r\n", len(request.body))
	case mitmFramingChunked:
		buffer.WriteString("Transfer-Encoding: chunked\r\n")
	}
	buffer.WriteString("\r\n")
	switch request.framing {
	case mitmFramingLength:
		buffer.Write(request.body)
	case mitmFramingChunked:
		fmt.Fprintf(&buffer, "%x\r\n", len(request.body))
		buffer.Write(request.body)
		buffer.WriteString("\r\n0\r\n\r\n")
	}
	// The oversized-header row is answered and closed while the client is
	// still writing, so the write is allowed to fail: what the row records is
	// what came back, not whether all of it went out.
	_, writeErr := conn.Write(buffer.Bytes())

	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		return mitmAnswerRow{}, fmt.Errorf("read: %w (write: %v)", err, writeErr)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, mitmCap))
	if err != nil {
		return mitmAnswerRow{}, fmt.Errorf("body: %w", err)
	}
	return mitmAnswerRow{
		Status:  response.StatusCode,
		Headers: mitmHeaders(response.Header),
		Body:    pointHex(body),
		Framing: mitmFramingOf(response.TransferEncoding, response.ContentLength),
	}, nil
}

// mitmH2Exchange sends the same script over h2, which has no framing to choose:
// a row that names chunked framing is not expressible here, and none does.
func mitmH2Exchange(conn net.Conn, sample mitmSessionSample) ([]mitmAnswerRow, error) {
	transport := &http2.Transport{
		DialTLSContext: func(context.Context, string, string, *tls.Config) (net.Conn, error) {
			return conn, nil
		},
	}
	defer transport.CloseIdleConnections()
	var answers []mitmAnswerRow
	for _, request := range sample.requests {
		if request.framing == mitmFramingChunked {
			return nil, errors.New("h2 has no chunked request framing")
		}
		authority := request.authority
		if authority == "" {
			authority = mitmServerName
		}
		var body io.Reader
		if request.framing == mitmFramingLength {
			body = bytes.NewReader(request.body)
		}
		out, err := http.NewRequest(request.method, "https://"+authority+":443"+request.target, body)
		if err != nil {
			return nil, err
		}
		// The authority is set as a field rather than taken from the URL,
		// which would carry the port the harness listens on.
		out.Host = authority
		for _, header := range request.headers {
			out.Header.Add(header[0], header[1])
		}
		response, err := transport.RoundTrip(out)
		if err != nil {
			return nil, fmt.Errorf("round trip: %w", err)
		}
		answer, err := io.ReadAll(io.LimitReader(response.Body, mitmCap))
		_ = response.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("body: %w", err)
		}
		answers = append(answers, mitmAnswerRow{
			Status:  response.StatusCode,
			Headers: mitmHeaders(response.Header),
			Body:    pointHex(answer),
			Framing: mitmFramingOf(response.TransferEncoding, response.ContentLength),
		})
	}
	return answers, nil
}

// ------------------------------------------------------------------ the pieces

// mitmRecorder is the hooks the rows install: it writes down every call and
// decides with the row's script.
type mitmRecorder struct {
	mu           sync.Mutex
	events       []mitmTraceEvent
	denyRequest  bool
	denyResponse bool
}

// errMITMDenied is what a hook returns to refuse; the engine turns any error
// into a 403.
var errMITMDenied = errors.New("the harness denies this traffic")

func (r *mitmRecorder) OnConnect(context.Context, *corenet.MITMConnectInfo) error { return nil }

func (r *mitmRecorder) OnRequest(_ context.Context, info *corenet.MITMRequestInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, mitmTraceEvent{
		Event:   "request",
		Method:  info.Method,
		Scheme:  info.Scheme,
		Host:    info.Host,
		Path:    info.Path,
		Headers: mitmHeaders(info.Header),
		Body:    pointHex(info.Body),
	})
	if r.denyRequest {
		return errMITMDenied
	}
	return nil
}

func (r *mitmRecorder) OnResponse(_ context.Context, info *corenet.MITMResponseInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, mitmTraceEvent{
		Event:   "response",
		Status:  info.StatusCode,
		Headers: mitmHeaders(info.Header),
		Body:    pointHex(info.Body),
	})
	if r.denyResponse {
		return errMITMDenied
	}
	return nil
}

func (r *mitmRecorder) snapshot() []mitmTraceEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]mitmTraceEvent{}, r.events...)
}

// mitmStub is the target: a TLS server holding a certificate the engine trusts,
// answering every request with the row's script and writing down what it saw.
type mitmStub struct {
	listener net.Listener
	server   *http.Server
	answer   mitmAnswerSample
	mu       sync.Mutex
	seen     []mitmSeenRow
}

func startMITMStub(answer mitmAnswerSample, leaf tls.Certificate) (*mitmStub, error) {
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{leaf},
	})
	if err != nil {
		return nil, err
	}
	stub := &mitmStub{listener: listener, answer: answer}
	stub.server = &http.Server{
		Handler: http.HandlerFunc(stub.handle),
		// The rejected-certificate row fails a handshake on purpose; the
		// row's answer is the 502 it produces, not net/http's log line
		// about it.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go func() { _ = stub.server.Serve(listener) }()
	return stub, nil
}

func (s *mitmStub) address() string { return s.listener.Addr().String() }

func (s *mitmStub) stop() {
	_ = s.server.Close()
	_ = s.listener.Close()
}

func (s *mitmStub) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, mitmCap))
	seen := mitmSeenRow{
		Method:    r.Method,
		Target:    r.URL.RequestURI(),
		Authority: r.Host,
		Proto:     r.Proto,
		Headers:   mitmHeaders(r.Header),
		Body:      pointHex(body),
		Framing:   mitmFramingOf(r.TransferEncoding, r.ContentLength),
	}
	if r.TLS != nil {
		seen.SNI = r.TLS.ServerName
	}
	s.mu.Lock()
	s.seen = append(s.seen, seen)
	s.mu.Unlock()

	if s.answer.status == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for _, header := range s.answer.headers {
		w.Header().Add(header[0], header[1])
	}
	if s.answer.framing == mitmFramingLength {
		w.Header().Set("Content-Length", strconv.Itoa(len(s.answer.body)))
	}
	w.WriteHeader(s.answer.status)
	if s.answer.framing == mitmFramingChunked {
		// Flushing before the body is what makes the answer chunked: with
		// no declared length and the buffer already on its way, net/http
		// has nothing left to measure.
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	if len(s.answer.body) > 0 {
		_, _ = w.Write(s.answer.body)
	}
}

func (s *mitmStub) snapshot() []mitmSeenRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mitmSeenRow{}, s.seen...)
}

// ------------------------------------------------------------------ the shapes

func mitmRequestRows(samples []mitmRequestSample) []mitmRequestRow {
	rows := make([]mitmRequestRow, 0, len(samples))
	for _, sample := range samples {
		authority := sample.authority
		if authority == "" {
			authority = mitmServerName
		}
		row := mitmRequestRow{
			Method:    sample.method,
			Target:    sample.target,
			Authority: authority,
			Headers:   mitmHeaderMap(sample.headers),
			Framing:   sample.framing,
		}
		if len(sample.body) > 0 {
			row.Body = hexOf(sample.body)
		}
		rows = append(rows, row)
	}
	return rows
}

func (s mitmAnswerSample) row() mitmAnswerRow {
	row := mitmAnswerRow{
		Status:  s.status,
		Headers: mitmHeaderMap(s.headers),
		Framing: s.framing,
	}
	row.Body = pointHex(s.body)
	return row
}

func mitmHookName(name string) string {
	if name == mitmHookDeny {
		return mitmHookDeny
	}
	return mitmHookAllow
}

// mitmFramingOf names how a message was delimited: chunked if it said so, a
// length if it declared one, and nothing at all if it carried no body.
func mitmFramingOf(transferEncoding []string, contentLength int64) string {
	for _, encoding := range transferEncoding {
		if strings.EqualFold(encoding, "chunked") {
			return mitmFramingChunked
		}
	}
	if contentLength > 0 {
		return mitmFramingLength
	}
	return mitmFramingNone
}

// mitmMasked are the headers a row does not compare. `date` is a clock and both
// stacks stamp it; `connection` is the writer's framing decision, which is the
// writer's to make; `x-pad` is the harness's own filler, there to reach the
// header limit.
var mitmMasked = map[string]bool{"date": true, "connection": true, "x-pad": true}

// mitmHeaders lowercases and sorts a header map and drops the masked ones. The
// values keep their order: two values for one name are ordered on the wire.
func mitmHeaders(header http.Header) map[string][]string {
	if len(header) == 0 {
		return nil
	}
	masked := make(map[string][]string, len(header))
	for name, values := range header {
		lower := strings.ToLower(name)
		if mitmMasked[lower] {
			continue
		}
		masked[lower] = append([]string(nil), values...)
	}
	if len(masked) == 0 {
		return nil
	}
	return masked
}

// mitmHeaderMap is mitmHeaders for a script's ordered pairs, which may repeat a
// name.
func mitmHeaderMap(headers [][2]string) map[string][]string {
	if len(headers) == 0 {
		return nil
	}
	masked := make(map[string][]string, len(headers))
	for _, header := range headers {
		if mitmMasked[header[0]] {
			continue
		}
		masked[header[0]] = append(masked[header[0]], header[1])
	}
	if len(masked) == 0 {
		return nil
	}
	return masked
}

// mitmSortedHeaders is how a script's header map is put on the wire: by name,
// which is the order both stacks would send anyway.
func mitmSortedHeaders(headers map[string][]string) [][2]string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	sorted := make([][2]string, 0, len(headers))
	for _, name := range names {
		for _, value := range headers[name] {
			sorted = append(sorted, [2]string{name, value})
		}
	}
	return sorted
}

// pointHex is a body as the dump spells it: nil is "there was no body", the
// empty string is an empty one.
func pointHex(body []byte) *string {
	if body == nil {
		return nil
	}
	encoded := hexOf(body)
	return &encoded
}

func hexOrEmpty(body []byte) string {
	if body == nil {
		return ""
	}
	return hexOf(body)
}
