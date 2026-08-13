package sandbox

import (
	"context"
	"errors"
	"time"
)

// Session errors. They are plain sentinels: callers distinguish them
// with errors.Is rather than through errdefs classification, because
// neither is a policy refusal — one is a handle-lifecycle state and the
// other is a buffering guarantee that cannot be recovered by retrying.
var (
	// ErrSessionClosed is returned by Read/Write/Resize/Terminate after
	// the session's Close has run. Wait remains usable: the exit status
	// is cached and reaping already completed.
	ErrSessionClosed = errors.New("sandbox: session is closed")

	// ErrSequenceGap is returned by Read when afterSeq points into
	// output that the bounded replay buffer already dropped. The
	// caller must start over from SessionInfo-retrievable state or
	// abandon the replay; retrying with the same cursor never helps.
	ErrSequenceGap = errors.New("sandbox: output sequence gap; buffered output was truncated")
)

// SessionSpec describes one interactive or streaming process session.
//
// Field semantics:
//
//   - ID: caller-supplied unique identifier. Empty means the manager
//     generates one (returned on the Session handle). Duplicate IDs
//     are rejected with errdefs.Conflict while the earlier session is
//     still open.
//   - Argv: the command and its arguments; Argv[0] is the executable.
//     An empty slice is a Validation error.
//   - TTY: request a pseudo-terminal. The child then owns the
//     controlling terminal, stdout/stderr are merged into the single
//     SessionStreamTTY stream, and Resize applies to the pty window.
//     False runs the child on pipes with separate stdout/stderr
//     streams.
//   - Rows/Cols: initial pty window size (TTY only). Non-positive
//     values default to 24x80.
//   - Opts: the same policy surface as Runner.Exec (WorkDir, Env, Net,
//     Resources, Timeout). Policy is fixed at Start; Read/Write/Resize
//     never re-negotiate it.
type SessionSpec struct {
	ID   string
	Argv []string
	TTY  bool
	Rows int
	Cols int
	Opts ExecOptions
}

// SessionStream identifies which output stream a chunk belongs to.
// Non-TTY sessions carry SessionStreamStdout / SessionStreamStderr;
// TTY sessions carry only SessionStreamTTY (the pty merges them).
type SessionStream int

const (
	SessionStreamStdout SessionStream = iota
	SessionStreamStderr
	SessionStreamTTY
)

func (s SessionStream) String() string {
	switch s {
	case SessionStreamStdout:
		return "stdout"
	case SessionStreamStderr:
		return "stderr"
	case SessionStreamTTY:
		return "tty"
	default:
		return "unknown"
	}
}

// OutputChunk is one contiguous run of bytes from one stream. Seq is
// the sequence number of the chunk's first byte.
type OutputChunk struct {
	Seq    int64
	Stream SessionStream
	Data   []byte
}

// SessionOutput is one Read result. NextSeq is the cursor the caller
// passes as afterSeq on the next Read (exclusive: output before
// NextSeq has been returned). EOF reports that no further output will
// ever arrive — the process exited and every buffered byte has been
// returned up to NextSeq. Data remains replayable until Close even
// after EOF.
type SessionOutput struct {
	NextSeq int64
	Chunks  []OutputChunk
	EOF     bool
}

// SessionExitReason classifies why the process ended.
type SessionExitReason int

const (
	// SessionExited is a normal exit, including a non-zero exit code.
	SessionExited SessionExitReason = iota
	// SessionSignaled means the OS reported death by signal (and the
	// session was not the one that sent it).
	SessionSignaled
	// SessionTerminated means Terminate stopped the session.
	SessionTerminated
	// SessionTimedOut means ExecOptions.Timeout elapsed and the
	// session was killed; Wait also returns an errdefs timeout error.
	SessionTimedOut
	// SessionBudgetExceeded means a resource cap (MemoryBytes /
	// CPUMillicores) killed the session; Wait also returns an errdefs
	// BudgetExceeded error.
	SessionBudgetExceeded
	// SessionUnenforceable means the cap watcher lost its ability to
	// sample and killed the session rather than run it unguarded; Wait
	// also returns an errdefs NotAvailable error.
	SessionUnenforceable
)

func (r SessionExitReason) String() string {
	switch r {
	case SessionExited:
		return "exited"
	case SessionSignaled:
		return "signaled"
	case SessionTerminated:
		return "terminated"
	case SessionTimedOut:
		return "timed_out"
	case SessionBudgetExceeded:
		return "budget_exceeded"
	case SessionUnenforceable:
		return "unenforceable"
	default:
		return "unknown"
	}
}

// SessionExit is the final outcome of a session. Code is the process
// exit code (0 on success), or -1 when the reason is not an ordinary
// exit. Signal carries the terminating signal for SessionSignaled.
type SessionExit struct {
	Code   int
	Signal int
	Reason SessionExitReason
}

// SessionInfo is a snapshot of one managed session for List.
type SessionInfo struct {
	ID        string
	Argv      []string
	TTY       bool
	PID       int
	StartedAt time.Time
	Running   bool
	Exit      *SessionExit
}

// Session is the unified, tool-facing handle to one running process.
// The zero state is never valid: a Session comes from Runner.Start.
//
// Unlike the old capability-by-interface model, every optional
// capability (Signal, Watch) is part of the interface itself: an
// unsupported operation fails with errdefs.NotAvailable, and
// Capabilities declares what this particular session supports. Tools
// never need XxxOf discovery helpers, and a remote Session proxy can
// report the same data the server declared.
//
// Lifecycle contract:
//
//   - Read uses an append-only output log. afterSeq is an exclusive
//     cursor; each call returns at most maxBytes bytes and advances
//     NextSeq. If the bounded buffer already dropped output at
//     afterSeq, Read fails with ErrSequenceGap. Read blocks until data
//     is available, EOF, or ctx is done. Output remains readable until
//     Close, including after Wait.
//   - Write sends raw bytes to the child (stdin pipe or pty master).
//     It writes all data or fails; a blocked child can block Write past
//     ctx cancellation.
//   - Resize is only valid for TTY sessions; pipe sessions return
//     errdefs.NotAvailable.
//   - Terminate sends SIGTERM, then SIGKILL after a short grace period
//     (or when ctx is done). It is idempotent on an exited process and
//     leaves the output log readable.
//   - Wait blocks until the process exits (or ctx is done) and returns
//     the cached outcome; it is safe to call repeatedly and after
//     Close.
//   - Close terminates a still-running session, reaps it, and releases
//     the output log. Close is idempotent; the manager forgets the
//     session so it no longer appears in List.
//   - Signal interrupts the process (Ctrl-C semantics); whether the
//     session stays usable after the signal is backend-dependent.
//   - Watch subscribes one independent bounded queue that replays the
//     retained output before delivering live events; pull-based Read
//     stays the recovery path after SessionEventLag.
type Session interface {
	ID() string
	PID() int
	Read(ctx context.Context, afterSeq int64, maxBytes int) (SessionOutput, error)
	Write(ctx context.Context, data []byte) error
	// CloseInput closes the process's stdin after all input has been
	// written. Non-TTY sessions support it; TTY sessions return
	// errdefs.NotAvailable.
	CloseInput() error
	Resize(ctx context.Context, rows, cols int) error
	Signal(ctx context.Context, sig SessionSignal) error
	Terminate(ctx context.Context) error
	Wait(ctx context.Context) (SessionExit, error)
	Watch(ctx context.Context) (SessionWatcher, error)
	Close() error
	Capabilities() SessionCapabilities
}

// SessionCapabilities is the per-session capability declaration. It is
// a subset of what the backend advertised through
// [Runner.Capabilities] — e.g. a backend that supports pty sessions
// still returns TTY=false for a pipe session. Zero values mean the
// corresponding operation returns errdefs.NotAvailable.
type SessionCapabilities struct {
	TTY    bool // Resize + merged TTY output; CloseInput is NotAvailable
	Signal bool // Signal delivery
	Events bool // Watch push streams
}

// SessionSignal is a soft signal a Session can receive. Unlike
// Terminate, a signal interrupts: the process may catch it and
// continue, and the session stays usable.
type SessionSignal int

const (
	// SessionSignalInterrupt is Ctrl-C semantics: VINTR on TTY
	// sessions (the terminal driver signals the foreground process
	// group), SIGINT to the whole group on pipe sessions.
	SessionSignalInterrupt SessionSignal = iota
)

func (s SessionSignal) String() string {
	switch s {
	case SessionSignalInterrupt:
		return "interrupt"
	default:
		return "unknown"
	}
}

// SessionEventType classifies one pushed process event.
type SessionEventType int

const (
	// SessionEventOutput carries one output chunk (Seq = the chunk's
	// first byte; Data references the process's immutable buffer).
	SessionEventOutput SessionEventType = iota
	// SessionEventExited carries the final exit; Seq is the completion
	// cursor (all output has been emitted).
	SessionEventExited
	// SessionEventClosed is emitted when the session is Closed; the
	// Events channel closes right after it.
	SessionEventClosed
	// SessionEventLag means the subscriber's bounded queue overflowed.
	// Seq is the first missed byte cursor: the consumer must
	// Read(afterSeq=Lag.Seq) to fill the gap. The watcher closes
	// immediately after this event — re-Watch to resume live delivery.
	SessionEventLag
)

func (t SessionEventType) String() string {
	switch t {
	case SessionEventOutput:
		return "output"
	case SessionEventExited:
		return "exited"
	case SessionEventClosed:
		return "closed"
	case SessionEventLag:
		return "lag"
	default:
		return "unknown"
	}
}

// SessionEvent is one pushed event. Field validity follows Type:
// Output fills Seq/Stream/Data; Exited fills Seq/Exit; Lag fills Seq;
// Closed fills Seq only.
type SessionEvent struct {
	Seq    int64
	Type   SessionEventType
	Stream SessionStream
	Data   []byte
	Exit   *SessionExit
}

// SessionWatcher is one subscription to a Session's event stream.
// Events delivers replay-then-live events in seq order. The channel
// closes when ctx cancels, when Close is called, or right after the
// process's Closed event.
type SessionWatcher interface {
	Events() <-chan SessionEvent
	Close() error
}
