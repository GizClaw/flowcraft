// Package wire is the message format a remote sandbox speaks: the request
// mirrors, the response types it reuses, and the error envelope that carries a
// classification across a process boundary.
//
// # What is mirrored and what is reused
//
// Responses are the contract's own types — [sandbox.SessionOutput],
// [sandbox.SessionInfo], [sandbox.Capabilities], [sandbox.JournalBatch] — because
// they are read-only values with no absent-versus-empty ambiguity: a chunk's
// bytes, a cursor, an exit reason. Re-mirroring them would add a conversion layer
// that can only introduce bugs, and every backend change would have to be made
// twice.
//
// Requests are mirrored, because the policy types they carry do have that
// ambiguity and JSON flattens it. [sandbox.EnvPolicy] is the case that matters:
// Allow == nil means "inherit the host environment" while Allow == []string{}
// means "inherit nothing", and a plain []string field cannot tell the two apart
// after a round trip. [Env.Allow] is a pointer for exactly that reason.
//
// # The rules this format keeps
//
//   - Output is a cursor, not a stream. A read carries after_seq and answers with
//     a sequence range; the transport may hold the request open, but nothing here
//     depends on a persistent connection. A reconnect re-reads from a cursor.
//   - The session id is opaque. It is minted by the server and never parsed by
//     the client; nothing in this package assumes it has a shape.
//   - Version is a single integer, negotiated once by [Hello]. It exists so a
//     server and its clients can be deployed separately, not as a general-purpose
//     negotiation scheme.
//   - A failure travels as [Error]: an errdefs kind plus, for the two plain
//     sentinels the session contract defines, a code. See error.go for why both.
//
// Nothing here validates policy. The server does that when it starts a session;
// a wire decoder that rejected a policy would be a second place for the rules to
// drift, and the client is not the authority on what a backend can enforce.
package wire

import (
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// Version is the message format generation this package speaks. It changes when
// a field's meaning changes, not when a field is added: unknown fields are
// ignored, which is what lets a new client talk to an old server for additions.
const Version = 1

// Hello is the first exchange: it fixes the version and publishes the server's
// own declaration, so the client never guesses what the far end can do.
//
// Capabilities is the server's value verbatim. A client must not add to it: the
// declaration is what lets a caller decide before trying, and a proxy that
// exaggerates it turns that decision into a failed call.
type Hello struct {
	Version      int                  `json:"version"`
	Server       string               `json:"server,omitempty"`
	Capabilities sandbox.Capabilities `json:"capabilities"`
}

// Spec is the wire form of [sandbox.SessionSpec].
type Spec struct {
	ID    string   `json:"id,omitempty"`
	Argv  []string `json:"argv"`
	TTY   bool     `json:"tty,omitempty"`
	Rows  int      `json:"rows,omitempty"`
	Cols  int      `json:"cols,omitempty"`
	Stdin bool     `json:"stdin,omitempty"`
	Opts  Options  `json:"opts"`
}

// Options is the wire form of [sandbox.ExecOptions]. Stdin is the one-shot
// input; nil and empty are equivalent here because both mean "no input side",
// which is a different field ([Spec.Stdin]).
type Options struct {
	WorkDir    string                 `json:"workdir,omitempty"`
	Stdin      []byte                 `json:"stdin,omitempty"`
	TimeoutMS  int64                  `json:"timeout_ms,omitempty"`
	Env        Env                    `json:"env"`
	Net        permission.NetPolicy   `json:"net"`
	Permission *permission.Policy     `json:"permission,omitempty"`
	Resources  sandbox.ResourceLimits `json:"resources"`
}

// Env is the wire form of [sandbox.EnvPolicy]. Allow is a pointer so "inherit the
// host environment" (absent) stays distinct from "inherit nothing" (present and
// empty) — the two are different decisions, and a plain slice loses which one was
// made.
type Env struct {
	Allow  *[]string         `json:"allow,omitempty"`
	Inject map[string]string `json:"inject,omitempty"`
}

// StartResponse answers a start: the id the server minted, the process id on the
// server's host (informational only — it means nothing on the client side), and
// the per-session capabilities the handle really has.
type StartResponse struct {
	ID           string                      `json:"id"`
	PID          int                         `json:"pid,omitempty"`
	Capabilities sandbox.SessionCapabilities `json:"capabilities"`
}

// SessionRef names a session in a request that carries nothing else. The
// session id is in the body rather than the path because the transport addresses
// operations, not resources: one endpoint per op keeps a transport free of
// routing rules.
type SessionRef struct {
	ID string `json:"id"`
}

// JournalRequest asks for the next slice of the journal: the events after
// AfterSeq, at most Max of them. It never waits; a journal is a log, not a
// stream.
//
// HoldMS is how long the server may keep the request open waiting for output; a
// server that reaches it answers with an empty batch and Idle set. AfterSeq is
// exclusive, exactly like [sandbox.Session.Read].
type ReadRequest struct {
	ID       string `json:"id"`
	AfterSeq int64  `json:"after_seq"`
	MaxBytes int    `json:"max_bytes,omitempty"`
	HoldMS   int    `json:"hold_ms,omitempty"`
}

// ReadResponse is one read result. Idle reports that the server returned because
// HoldMS elapsed rather than because the process produced anything; the batch is
// then empty and the cursor unchanged.
type ReadResponse struct {
	Output sandbox.SessionOutput `json:"output"`
	Idle   bool                  `json:"idle,omitempty"`
}

type JournalRequest struct {
	AfterSeq int64 `json:"after_seq"`
	Max      int   `json:"max,omitempty"`
}

// WriteRequest carries the bytes to send to the process.
type WriteRequest struct {
	ID   string `json:"id"`
	Data []byte `json:"data"`
}

// WriteResponse reports how many bytes were accepted. A client must not retry an
// unacknowledged write: the bytes may already have reached the child.
type WriteResponse struct {
	BytesWritten int `json:"bytes_written"`
}

// ResizeRequest applies to a TTY session only.
type ResizeRequest struct {
	ID   string `json:"id"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

// SignalRequest carries a [sandbox.SessionSignal].
type SignalRequest struct {
	ID     string `json:"id"`
	Signal int    `json:"signal"`
}

// WaitRequest asks for the exit, optionally holding until it happens.
type WaitRequest struct {
	ID     string `json:"id"`
	HoldMS int    `json:"hold_ms,omitempty"`
}

// EventsRequest opens the session's event stream.
//
// It carries no cursor on purpose: the contract's Session.Watch has none either
// — it replays the retained output from its start and then follows live, and a
// consumer that fell behind recovers with a Read from the seq a Lag event names.
// Inventing a resume cursor here would be a second, weaker recovery path.
type EventsRequest struct {
	ID string `json:"id"`
}

// Event is one pushed session event.
//
// Type is one of "output", "exited", "closed", "lag"; the fields that matter
// follow it, exactly like sandbox.SessionEvent.
type Event struct {
	Seq    int64                `json:"seq"`
	Type   string               `json:"type"`
	Stream int                  `json:"stream,omitempty"`
	Data   []byte               `json:"data,omitempty"`
	Exit   *sandbox.SessionExit `json:"exit,omitempty"`
}

// CloseResponse answers a close. Close terminates and reaps, so the exit it
// produced is the answer: a client that closed a session can still report how it
// ended, which is what the contract promises a handle does after Close.
type CloseResponse struct {
	Exit *sandbox.SessionExit `json:"exit,omitempty"`
}

// WaitResponse answers a wait. Running reports that the server returned because
// the hold elapsed, not because the process exited: the client keeps waiting.
// Exit is set only when Running is false.
type WaitResponse struct {
	Exit    *sandbox.SessionExit `json:"exit,omitempty"`
	Running bool                 `json:"running,omitempty"`
}

// FromSpec converts a contract session spec into its wire form.
func FromSpec(spec sandbox.SessionSpec) Spec {
	return Spec{
		ID:    spec.ID,
		Argv:  append([]string(nil), spec.Argv...),
		TTY:   spec.TTY,
		Rows:  spec.Rows,
		Cols:  spec.Cols,
		Stdin: spec.Stdin,
		Opts:  fromOptions(spec.Opts),
	}
}

// ToSpec is the inverse of [FromSpec].
func ToSpec(w Spec) sandbox.SessionSpec {
	return sandbox.SessionSpec{
		ID:    w.ID,
		Argv:  append([]string(nil), w.Argv...),
		TTY:   w.TTY,
		Rows:  w.Rows,
		Cols:  w.Cols,
		Stdin: w.Stdin,
		Opts:  toOptions(w.Opts),
	}
}

func fromOptions(opts sandbox.ExecOptions) Options {
	out := Options{
		WorkDir:    opts.WorkDir,
		Stdin:      opts.Stdin,
		TimeoutMS:  opts.Timeout.Milliseconds(),
		Env:        fromEnv(opts.Env),
		Net:        opts.Net,
		Permission: opts.Permission,
		Resources:  opts.Resources,
	}
	return out
}

func toOptions(w Options) sandbox.ExecOptions {
	opts := sandbox.ExecOptions{
		WorkDir:    w.WorkDir,
		Stdin:      w.Stdin,
		Env:        toEnv(w.Env),
		Net:        w.Net,
		Permission: w.Permission,
		Resources:  w.Resources,
	}
	// A negative duration is not a deadline; treat it as "none" rather than
	// handing a backend a value it would have to reinterpret.
	if w.TimeoutMS > 0 {
		opts.Timeout = time.Duration(w.TimeoutMS) * time.Millisecond
	}
	return opts
}

func fromEnv(p sandbox.EnvPolicy) Env {
	out := Env{Inject: p.Inject}
	if p.Allow != nil {
		allow := p.Allow
		out.Allow = &allow
	}
	return out
}

func toEnv(w Env) sandbox.EnvPolicy {
	out := sandbox.EnvPolicy{Inject: w.Inject}
	if w.Allow != nil {
		out.Allow = *w.Allow
	}
	return out
}

// Event type spellings. They are wire values, so they are strings a log can carry
// and a foreign implementation can read.
const (
	EventOutput = "output"
	EventExited = "exited"
	EventClosed = "closed"
	EventLag    = "lag"
)

// FromEvent converts a session event into its wire form.
func FromEvent(ev sandbox.SessionEvent) Event {
	out := Event{
		Seq:    ev.Seq,
		Stream: int(ev.Stream),
		Data:   ev.Data,
		Exit:   ev.Exit,
	}
	switch ev.Type {
	case sandbox.SessionEventExited:
		out.Type = EventExited
	case sandbox.SessionEventClosed:
		out.Type = EventClosed
	case sandbox.SessionEventLag:
		out.Type = EventLag
	default:
		out.Type = EventOutput
	}
	return out
}

// ToEvent converts a wire event back into a session event. An unknown type is
// reported rather than mapped to output: a receiver that cannot classify an
// event must not turn it into bytes.
func ToEvent(w Event) (sandbox.SessionEvent, error) {
	out := sandbox.SessionEvent{
		Seq:    w.Seq,
		Stream: sandbox.SessionStream(w.Stream),
		Data:   w.Data,
		Exit:   w.Exit,
	}
	switch w.Type {
	case EventOutput:
		out.Type = sandbox.SessionEventOutput
	case EventExited:
		out.Type = sandbox.SessionEventExited
	case EventClosed:
		out.Type = sandbox.SessionEventClosed
	case EventLag:
		out.Type = sandbox.SessionEventLag
	default:
		return sandbox.SessionEvent{}, errdefs.Validationf(
			"wire: unknown session event type %q", w.Type)
	}
	return out, nil
}
