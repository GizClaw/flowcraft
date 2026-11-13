package remote

import (
	"context"
	"errors"
	"io"
)

// Op names one request the server understands. It doubles as the URL path
// segment in the HTTP transport, which is why the values are stable strings
// rather than an iota: they are part of the wire contract.
type Op string

// The operations a server serves. They are listed here rather than spelled at
// each call site so a transport can dispatch on a single closed set.
const (
	OpHello      Op = "hello"
	OpStart      Op = "start"
	OpAttach     Op = "attach"
	OpList       Op = "list"
	OpRead       Op = "read"
	OpWrite      Op = "write"
	OpCloseInput Op = "close-input"
	OpResize     Op = "resize"
	OpSignal     Op = "signal"
	OpTerminate  Op = "terminate"
	OpWait       Op = "wait"
	OpClose      Op = "close"
	OpJournal    Op = "journal"
	// OpEvents is the one op that streams: it answers with Server-Sent Events
	// rather than a single decoded body.
	OpEvents Op = "events"
)

// Transport carries one message to a server and decodes the answer.
//
// It exists so the protocol is not welded to HTTP: the client and the server
// both speak request/response messages, and only this interface knows how they
// travel. A single bidirectional stream is the eventual shape for interactive
// traffic, and it can be added here without touching the session logic.
//
// Implementations must return an error built by the wire package for a failure
// the server reported (so errdefs.IsXxx and errors.Is keep working on the
// client), and a transport-level error for a failure that never reached the
// server.
type Transport interface {
	// Do sends op with the already-encoded request body and decodes the
	// response into res. A nil req means "no request body".
	Do(ctx context.Context, op Op, req []byte, res any) error

	// DoBytes sends op with the already-encoded request body and returns the
	// response body untouched. It exists for the read operation, whose payload is
	// a binary frame: decoding it inside the transport would mean teaching the
	// transport about payloads, and the client owns that format.
	DoBytes(ctx context.Context, op Op, req []byte) ([]byte, error)

	// Stream sends op and returns the response body as it arrives. It exists
	// for the one operation that pushes: the session event stream. The caller
	// closes the reader, which ends the request.
	Stream(ctx context.Context, op Op, req []byte) (io.ReadCloser, error)
}

// TransportError reports that a request never reached a decision: a dial
// failure, a reset connection, a stall. It is the only class a client repeats,
// which is why it is a type and not a message check — a failure the server
// answered, however transient-looking, is final for that operation.
type TransportError struct{ Err error }

func (e *TransportError) Error() string {
	return "sandbox-remote: transport: " + e.Err.Error()
}

// Unwrap keeps the underlying network error reachable for errors.Is/As.
func (e *TransportError) Unwrap() error { return e.Err }

// AsTransportError reports whether err is (or wraps) a transport failure.
func AsTransportError(err error) (*TransportError, bool) {
	var te *TransportError
	if errors.As(err, &te) {
		return te, true
	}
	return nil, false
}
