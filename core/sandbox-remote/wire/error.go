package wire

import (
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// Codes for the failures that are not errdefs classifications.
//
// [sandbox.ErrSessionClosed] and [sandbox.ErrSequenceGap] are plain sentinels by
// design (see the package doc of core/sandbox/session.go): neither is a policy
// refusal, one is a handle lifecycle state and the other a buffering guarantee,
// and callers tell them apart with errors.Is rather than by category. They
// therefore need their own spelling on the wire; a kind alone would arrive as
// "unclassified" and the client would stop distinguishing them.
const (
	CodeSessionClosed = "session_closed"
	CodeSequenceGap   = "sequence_gap"
)

// Error is the wire form of a failure: a classification (or a code), and the
// text the sender produced.
//
// A code wins over a kind. The two are disjoint in practice — the sentinels carry
// no errdefs marker — but if a sender wraps one in a classified error, the code
// is the more actionable half: it is the one a caller can branch on without
// knowing the classification vocabulary.
type Error struct {
	// Kind is an [errdefs.Kind] spelling. Empty means "no classification".
	Kind string `json:"kind,omitempty"`
	// Code is one of CodeSessionClosed or CodeSequenceGap.
	Code string `json:"code,omitempty"`
	// Message is the sender's error text, sent verbatim for the log the
	// receiver will write. It is never parsed.
	Message string `json:"message"`
}

// FromError converts an error into its wire form, or nil for a nil error.
func FromError(err error) *Error {
	if err == nil {
		return nil
	}
	out := &Error{Message: err.Error()}
	switch {
	case errors.Is(err, sandbox.ErrSessionClosed):
		out.Code = CodeSessionClosed
	case errors.Is(err, sandbox.ErrSequenceGap):
		out.Code = CodeSequenceGap
	default:
		out.Kind = errdefs.KindOf(err).String()
		if out.Kind == errdefs.KindNone.String() {
			// "none" is ParseKind's answer for the empty string; sending the
			// spelling would be noise on a field that is already optional.
			out.Kind = ""
		}
	}
	return out
}

// Err rebuilds the local error an [Error] stands for.
//
// The rebuilt value is not identical to the sender's — that is impossible across
// a process boundary — but it is observably equivalent where callers branch:
// errdefs.IsXxx and errors.Is agree with what the sender had.
func (e *Error) Err() error {
	if e == nil {
		return nil
	}
	switch e.Code {
	case CodeSessionClosed:
		return coded{sentinel: sandbox.ErrSessionClosed, msg: e.Message}
	case CodeSequenceGap:
		return coded{sentinel: sandbox.ErrSequenceGap, msg: e.Message}
	}
	kind, ok := errdefs.ParseKind(e.Kind)
	if !ok {
		// A name this build does not know is a protocol mismatch, not an
		// unclassified failure; say which one so the operator can see it.
		return fmt.Errorf("wire: unknown error kind %q: %s", e.Kind, e.Message)
	}
	if kind == errdefs.KindNone {
		return errors.New(e.Message)
	}
	return kind.Error(e.Message)
}

// coded is an error with the sender's text and a local sentinel behind it, so
// errors.Is works without the receiver having to parse the message.
type coded struct {
	sentinel error
	msg      string
}

func (c coded) Error() string { return c.msg }
func (c coded) Unwrap() error { return c.sentinel }
