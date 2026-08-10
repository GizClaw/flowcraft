package sandbox

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"

	"github.com/rs/xid"
)

// Process errors. They are plain sentinels: callers distinguish them
// with errors.Is rather than through errdefs classification, because
// neither is a policy refusal — one is a handle-lifecycle state and the
// other is a buffering guarantee that cannot be recovered by retrying.
var (
	// ErrProcessClosed is returned by Read/Write/Resize/Terminate after
	// the session's Close has run. Wait remains usable: the exit status
	// is cached and reaping already completed.
	ErrProcessClosed = errors.New("sandbox: process session is closed")

	// ErrSequenceGap is returned by Read when afterSeq points into
	// output that the bounded replay buffer already dropped. The
	// caller must start over from ProcessInfo-retrievable state or
	// abandon the replay; retrying with the same cursor never helps.
	ErrSequenceGap = errors.New("sandbox: output sequence gap; buffered output was truncated")
)

// registryWaitTimeout bounds the synchronous state sync after
// Terminate. A process that survived SIGKILL (rare kernel-level
// stall) must not wedge the manager forever.
const registryWaitTimeout = 10 * time.Second

// ProcessSpec describes one interactive or streaming process session.
//
// Field semantics:
//
//   - ID: caller-supplied unique identifier. Empty means the manager
//     generates one (returned on the Process handle). Duplicate IDs
//     are rejected with errdefs.Conflict while the earlier session is
//     still open.
//   - Argv: the command and its arguments; Argv[0] is the executable.
//     An empty slice is a Validation error.
//   - TTY: request a pseudo-terminal. The child then owns the
//     controlling terminal, stdout/stderr are merged into the single
//     ProcessStreamTTY stream, and Resize applies to the pty window.
//     False runs the child on pipes with separate stdout/stderr
//     streams.
//   - Rows/Cols: initial pty window size (TTY only). Non-positive
//     values default to 24x80.
//   - Opts: the same policy surface as Runner.Exec (WorkDir, Env, Net,
//     Resources, Timeout). Policy is fixed at Start; Read/Write/Resize
//     never re-negotiate it.
type ProcessSpec struct {
	ID   string
	Argv []string
	TTY  bool
	Rows int
	Cols int
	Opts ExecOptions
}

// ProcessStream identifies which output stream a chunk belongs to.
// Non-TTY sessions carry ProcessStreamStdout / ProcessStreamStderr;
// TTY sessions carry only ProcessStreamTTY (the pty merges them).
type ProcessStream int

const (
	ProcessStreamStdout ProcessStream = iota
	ProcessStreamStderr
	ProcessStreamTTY
)

func (s ProcessStream) String() string {
	switch s {
	case ProcessStreamStdout:
		return "stdout"
	case ProcessStreamStderr:
		return "stderr"
	case ProcessStreamTTY:
		return "tty"
	default:
		return "unknown"
	}
}

// OutputChunk is one contiguous run of bytes from one stream. Seq is
// the sequence number of the chunk's first byte.
type OutputChunk struct {
	Seq    int64
	Stream ProcessStream
	Data   []byte
}

// ProcessOutput is one Read result. NextSeq is the cursor the caller
// passes as afterSeq on the next Read (exclusive: output before
// NextSeq has been returned). EOF reports that no further output will
// ever arrive — the process exited and every buffered byte has been
// returned up to NextSeq. Data remains replayable until Close even
// after EOF.
type ProcessOutput struct {
	NextSeq int64
	Chunks  []OutputChunk
	EOF     bool
}

// ProcessExitReason classifies why the process ended.
type ProcessExitReason int

const (
	// ProcessExited is a normal exit, including a non-zero exit code.
	ProcessExited ProcessExitReason = iota
	// ProcessSignaled means the OS reported death by signal (and the
	// session was not the one that sent it).
	ProcessSignaled
	// ProcessTerminated means Terminate stopped the session.
	ProcessTerminated
	// ProcessTimedOut means ExecOptions.Timeout elapsed and the
	// session was killed; Wait also returns an errdefs timeout error.
	ProcessTimedOut
	// ProcessBudgetExceeded means a resource cap (MemoryBytes /
	// CPUMillicores) killed the session; Wait also returns an errdefs
	// BudgetExceeded error.
	ProcessBudgetExceeded
	// ProcessUnenforceable means the cap watcher lost its ability to
	// sample and killed the session rather than run it unguarded; Wait
	// also returns an errdefs NotAvailable error.
	ProcessUnenforceable
)

func (r ProcessExitReason) String() string {
	switch r {
	case ProcessExited:
		return "exited"
	case ProcessSignaled:
		return "signaled"
	case ProcessTerminated:
		return "terminated"
	case ProcessTimedOut:
		return "timed_out"
	case ProcessBudgetExceeded:
		return "budget_exceeded"
	case ProcessUnenforceable:
		return "unenforceable"
	default:
		return "unknown"
	}
}

// ProcessExit is the final outcome of a session. Code is the process
// exit code (0 on success), or -1 when the reason is not an ordinary
// exit. Signal carries the terminating signal for ProcessSignaled.
type ProcessExit struct {
	Code   int
	Signal int
	Reason ProcessExitReason
}

// ProcessInfo is a snapshot of one managed session for List.
type ProcessInfo struct {
	ID        string
	Argv      []string
	TTY       bool
	PID       int
	StartedAt time.Time
	Running   bool
	Exit      *ProcessExit
}

// Process is a live session handle. The zero state is never valid: a
// Process comes from ProcessManager.Start.
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
type Process interface {
	ID() string
	PID() int
	Read(ctx context.Context, afterSeq int64, maxBytes int) (ProcessOutput, error)
	Write(ctx context.Context, data []byte) error
	Resize(ctx context.Context, rows, cols int) error
	Terminate(ctx context.Context) error
	Wait(ctx context.Context) (ProcessExit, error)
	Close() error
}

// ProcessSignal is a soft signal a Process can receive. Unlike
// Terminate, a signal interrupts: the process may catch it and
// continue, and the session stays usable.
type ProcessSignal int

const (
	// ProcessSignalInterrupt is Ctrl-C semantics: VINTR on TTY
	// sessions (the terminal driver signals the foreground process
	// group), SIGINT to the whole group on pipe sessions.
	ProcessSignalInterrupt ProcessSignal = iota
)

func (s ProcessSignal) String() string {
	switch s {
	case ProcessSignalInterrupt:
		return "interrupt"
	default:
		return "unknown"
	}
}

// ProcessSignaler is the optional signal capability of a Process.
// Discover it with ProcessSignalerOf. Backends without the capability
// must not implement it; Signal then surfaces NotAvailable instead of
// a silent no-op.
type ProcessSignaler interface {
	Signal(ctx context.Context, sig ProcessSignal) error
}

// ProcessSignalerOf returns the ProcessSignaler implemented by p, if
// any. It is the (T, bool) twin of ProcessManagerOf for process-level
// capabilities.
func ProcessSignalerOf(p Process) (ProcessSignaler, bool) {
	s, ok := p.(ProcessSignaler)
	return s, ok
}

// ProcessEventType classifies one pushed process event.
type ProcessEventType int

const (
	// ProcessEventOutput carries one output chunk (Seq = the chunk's
	// first byte; Data references the process's immutable buffer).
	ProcessEventOutput ProcessEventType = iota
	// ProcessEventExited carries the final exit; Seq is the completion
	// cursor (all output has been emitted).
	ProcessEventExited
	// ProcessEventClosed is emitted when the session is Closed; the
	// Events channel closes right after it.
	ProcessEventClosed
	// ProcessEventLag means the subscriber's bounded queue overflowed.
	// Seq is the first missed byte cursor: the consumer must
	// Read(afterSeq=Lag.Seq) to fill the gap. The watcher closes
	// immediately after this event — re-Watch to resume live delivery.
	ProcessEventLag
)

func (t ProcessEventType) String() string {
	switch t {
	case ProcessEventOutput:
		return "output"
	case ProcessEventExited:
		return "exited"
	case ProcessEventClosed:
		return "closed"
	case ProcessEventLag:
		return "lag"
	default:
		return "unknown"
	}
}

// ProcessEvent is one pushed event. Field validity follows Type:
// Output fills Seq/Stream/Data; Exited fills Seq/Exit; Lag fills Seq;
// Closed fills Seq only.
type ProcessEvent struct {
	Seq    int64
	Type   ProcessEventType
	Stream ProcessStream
	Data   []byte
	Exit   *ProcessExit
}

// ProcessWatcher is one subscription to a Process's event stream.
// Events delivers replay-then-live events in seq order. The channel
// closes when ctx cancels, when Close is called, or right after the
// process's Closed event.
type ProcessWatcher interface {
	Events() <-chan ProcessEvent
	Close() error
}

// ProcessEventSource is the optional push-capability of a Process:
// Watch subscribes one independent bounded queue that replays the
// retained output before delivering live events. Discover it with
// ProcessEventSourceOf. Pull-based Read is unchanged and stays the
// recovery path after ProcessEventLag.
type ProcessEventSource interface {
	Watch(ctx context.Context) (ProcessWatcher, error)
}

// ProcessEventSourceOf returns the ProcessEventSource implemented by
// p, if any.
func ProcessEventSourceOf(p Process) (ProcessEventSource, bool) {
	s, ok := p.(ProcessEventSource)
	return s, ok
}

// ProcessManager is the optional long-running-session capability of a
// sandbox. Runner.Exec remains the one-shot interface; a Runner that
// additionally implements ProcessManager can spawn interactive or
// streaming processes under the same ExecOptions policy. Backends that
// cannot spawn sessions must not implement this interface — callers
// discover capability with ProcessManagerOf and never see a silent
// downgrade, mirroring EnforcementOf.
//
// Policy is applied once, at Start: Read/Write/Resize/Terminate do not
// re-negotiate Env/Net/Resources. Unsupported requests (e.g. TTY on a
// backend without a pty) fail at Start with errdefs.NotAvailable.
type ProcessManager interface {
	Start(ctx context.Context, spec ProcessSpec) (Process, error)
	List(ctx context.Context) ([]ProcessInfo, error)
	Terminate(ctx context.Context, id string) error
}

// ProcessStarter implements one backend's spawn: it turns a ProcessSpec
// into a launched Process. It is the injection seam shared by every
// backend's ProcessManager (LocalRunner, seatbelt, bwrap) so session
// bookkeeping stays in one place.
type ProcessStarter func(ctx context.Context, spec ProcessSpec) (Process, error)

// ProcessManagerOf returns the ProcessManager implemented by r, or nil
// when the runner (including a nil one) does not support sessions. It
// is the ProcessManager twin of EnforcementOf and the canonical way to
// discover the optional capability on a decorated runner chain.
func ProcessManagerOf(r Runner) ProcessManager {
	if pm, ok := r.(ProcessManager); ok {
		return pm
	}
	return nil
}

// NewProcessRegistry returns a ProcessManager whose sessions are
// tracked in-process and started by starter. It implements the ID
// uniqueness / generation, List, Terminate-by-ID, and Close removal
// contract so every backend gets identical session semantics.
func NewProcessRegistry(starter ProcessStarter) ProcessManager {
	return &processRegistry{
		starter:  starter,
		sessions: make(map[string]*processRecord),
	}
}

type processRegistry struct {
	starter  ProcessStarter
	mu       sync.Mutex
	sessions map[string]*processRecord
}

type processRecord struct {
	id      string
	spec    ProcessSpec
	proc    Process
	pid     int
	started time.Time
	exited  bool
	exit    ProcessExit
	err     error
}

func (r *processRegistry) Start(ctx context.Context, spec ProcessSpec) (Process, error) {
	if r.starter == nil {
		return nil, errdefs.NotAvailablef("sandbox: process starter not configured")
	}
	if len(spec.Argv) == 0 {
		return nil, errdefs.Validationf("sandbox: ProcessSpec.Argv must name a command")
	}
	id := spec.ID
	if id == "" {
		id = xid.New().String()
	}

	rec := &processRecord{id: id, spec: spec, started: time.Now()}
	r.mu.Lock()
	if _, exists := r.sessions[id]; exists {
		r.mu.Unlock()
		return nil, errdefs.Conflictf("sandbox: process id %q already exists", id)
	}
	r.sessions[id] = rec
	r.mu.Unlock()

	// Resolve the ID before spawning so the backend's Process handle
	// and the registry record share one identifier.
	spec.ID = id
	proc, err := r.starter(ctx, spec)
	if err != nil {
		r.remove(id)
		return nil, err
	}
	if proc == nil {
		r.remove(id)
		return nil, errdefs.Internalf("sandbox: process starter returned a nil process")
	}

	r.mu.Lock()
	rec.proc = proc
	rec.pid = proc.PID()
	r.mu.Unlock()

	go r.track(id, proc)
	return &registryProcess{inner: proc, reg: r, id: id}, nil
}

func (r *processRegistry) track(id string, proc Process) {
	exit, err := proc.Wait(context.Background())
	r.mu.Lock()
	if rec := r.sessions[id]; rec != nil {
		rec.exited = true
		rec.exit = exit
		rec.err = err
	}
	r.mu.Unlock()
}

func (r *processRegistry) List(context.Context) ([]ProcessInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ProcessInfo, 0, len(r.sessions))
	for _, rec := range r.sessions {
		info := ProcessInfo{
			ID:        rec.id,
			Argv:      append([]string(nil), rec.spec.Argv...),
			TTY:       rec.spec.TTY,
			PID:       rec.pid,
			StartedAt: rec.started,
			Running:   !rec.exited,
		}
		if rec.exited {
			info.Exit = &rec.exit
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out, nil
}

func (r *processRegistry) Terminate(ctx context.Context, id string) error {
	r.mu.Lock()
	rec := r.sessions[id]
	r.mu.Unlock()
	if rec == nil {
		return errdefs.NotFoundf("sandbox: unknown process id %q", id)
	}
	if rec.exited {
		return nil
	}
	if rec.proc == nil {
		return errdefs.NotAvailablef("sandbox: process %q is still starting", id)
	}
	if err := rec.proc.Terminate(ctx); err != nil {
		return err
	}
	// Synchronise the record: List must reflect the termination as soon
	// as Terminate returns, not whenever the background tracker next
	// gets scheduled. Wait is already satisfied for a terminated
	// process, so this is a quick state sync, not a second reaping.
	waitCtx, cancel := context.WithTimeout(context.Background(), registryWaitTimeout)
	defer cancel()
	exit, err := rec.proc.Wait(waitCtx)
	r.mu.Lock()
	if cur := r.sessions[id]; cur == rec && !rec.exited {
		rec.exited = true
		rec.exit = exit
		rec.err = err
	}
	r.mu.Unlock()
	return nil
}

func (r *processRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
}

// registryProcess forwards to the underlying session and removes the
// registry record on Close. ID is the registry-resolved ID so callers
// always see the stable identifier, including manager-generated ones.
type registryProcess struct {
	inner Process
	reg   *processRegistry
	id    string
	once  sync.Once
}

func (p *registryProcess) ID() string { return p.id }
func (p *registryProcess) PID() int   { return p.inner.PID() }

func (p *registryProcess) Read(ctx context.Context, afterSeq int64, maxBytes int) (ProcessOutput, error) {
	return p.inner.Read(ctx, afterSeq, maxBytes)
}

func (p *registryProcess) Write(ctx context.Context, data []byte) error {
	return p.inner.Write(ctx, data)
}

func (p *registryProcess) Resize(ctx context.Context, rows, cols int) error {
	return p.inner.Resize(ctx, rows, cols)
}

func (p *registryProcess) Terminate(ctx context.Context) error {
	return p.inner.Terminate(ctx)
}

// Signal forwards the optional signal capability of the underlying
// session. registryProcess wraps inner as a named field, so methods
// are not promoted automatically; the wrapper must implement the
// optional interfaces itself for discovery to work on Start handles.
func (p *registryProcess) Signal(ctx context.Context, sig ProcessSignal) error {
	signaler, ok := ProcessSignalerOf(p.inner)
	if !ok {
		return errdefs.NotAvailablef("sandbox: process does not support signals")
	}
	return signaler.Signal(ctx, sig)
}

// Watch forwards the optional event-source capability of the
// underlying session, mirroring Signal.
func (p *registryProcess) Watch(ctx context.Context) (ProcessWatcher, error) {
	source, ok := ProcessEventSourceOf(p.inner)
	if !ok {
		return nil, errdefs.NotAvailablef("sandbox: process does not support event streams")
	}
	return source.Watch(ctx)
}

func (p *registryProcess) Wait(ctx context.Context) (ProcessExit, error) {
	return p.inner.Wait(ctx)
}

func (p *registryProcess) Close() error {
	err := p.inner.Close()
	p.once.Do(func() { p.reg.remove(p.id) })
	return err
}
