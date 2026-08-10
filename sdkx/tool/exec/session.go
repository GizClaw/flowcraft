package exec

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// SessionName is the canonical tool id of the interactive session
// driver, registered alongside [Name] by hosts that want one.
const SessionName = "exec_session"

const (
	defaultMaxBytes   = 4096
	maxReadBytes      = 1 << 20
	defaultSessionTTL = 30 * time.Minute
)

// SessionTool is the LLM-callable session driver. It holds the tool's
// own session registry (the injected ProcessManager tracks processes;
// the tool tracks which sessions this LLM context owns). Safe for
// concurrent use; call Close when the host shuts down so no session
// outlives the tool.
type SessionTool struct {
	pm             sandbox.ProcessManager
	defaultTimeout time.Duration
	ttl            time.Duration

	mu       sync.Mutex
	sessions map[string]*sessionState
	closed   bool

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

type sessionState struct {
	proc     sandbox.Process
	lastUsed time.Time
}

// SessionOption configures a [SessionTool] at construction time.
type SessionOption func(*SessionTool)

// WithSessionDefaultTimeout sets the sandbox session-lifetime timeout
// used when start does not supply timeout_seconds. Zero means "no
// tool-imposed default" — the session runs until it exits, is
// terminated, or is closed. Negative values are treated as zero.
func WithSessionDefaultTimeout(d time.Duration) SessionOption {
	return func(t *SessionTool) {
		if d > 0 {
			t.defaultTimeout = d
		}
	}
}

// WithSessionTTL sets how long an idle session is kept before the
// tool terminates it. Defaults to 30 minutes. Pass a non-positive
// value to disable expiry (sessions then live until close / process
// exit).
func WithSessionTTL(d time.Duration) SessionOption {
	return func(t *SessionTool) {
		t.ttl = d
	}
}

// NewSession constructs the exec_session tool. pm MUST be non-nil:
// there is no host-shell fallback and no silent downgrade to one-shot
// Exec.
func NewSession(pm sandbox.ProcessManager, opts ...SessionOption) (*SessionTool, error) {
	if pm == nil {
		return nil, errdefs.Validationf(
			"exec_session: sandbox.ProcessManager is required; use sandbox.ProcessManagerOf(runner) to discover it")
	}
	t := &SessionTool{
		pm:       pm,
		ttl:      defaultSessionTTL,
		sessions: make(map[string]*sessionState),
		stopCh:   make(chan struct{}),
	}
	for _, o := range opts {
		o(t)
	}
	if t.ttl > 0 {
		t.wg.Add(1)
		go t.reapLoop()
	}
	return t, nil
}

// MustNewSession is the panic-on-error variant of [NewSession].
func MustNewSession(pm sandbox.ProcessManager, opts ...SessionOption) *SessionTool {
	t, err := NewSession(pm, opts...)
	if err != nil {
		panic(err)
	}
	return t
}

// Close stops the idle reaper and terminates every session the tool
// owns, so no child process outlives the host. Close is idempotent;
// after Close, Execute returns NotAvailable.
func (t *SessionTool) Close() error {
	t.stopOnce.Do(func() { close(t.stopCh) })
	t.wg.Wait()

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	sessions := t.sessions
	t.sessions = make(map[string]*sessionState)
	t.mu.Unlock()

	for _, s := range sessions {
		_ = s.proc.Close()
	}
	return nil
}

// Definition returns the model-facing schema.
func (t *SessionTool) Definition() message.Definition {
	return message.DefineSchema(
		SessionName,
		"Start and drive an interactive or streaming process session "+
			"inside the agent's sandbox. Sessions keep running between "+
			"calls and output is replayed with a sequence cursor "+
			"(after_seq / next_seq), so you can drive REPLs, TUIs, and "+
			"long-running processes step by step. Policy (env, network, "+
			"resources, approval) is fixed at start; an interactive "+
			"session is an all-or-nothing command channel, so prefer "+
			"the exec tool for one-shot commands.",
		message.ToolEnumProperty("action", "string",
			"Operation to perform. start creates a session and returns its session_id; read returns buffered output from after_seq; write sends input bytes; resize sets the pty window size; status reports whether the session is still running and its exit; terminate stops the process (output stays readable until close); close frees the session.",
			"start", "read", "write", "resize", "status", "terminate", "close"),
		message.ToolProperty("session_id", "string",
			"Session id returned by start. Required for every action except start."),
		message.ToolProperty("command", "string",
			"The program to run (required for start). Resolved against the sandbox's PATH policy."),
		message.ToolArrayProperty("args",
			"Arguments passed verbatim to the program (start).",
			message.Items("string")),
		message.ToolProperty("workdir", "string",
			"Working directory, relative to the sandbox root (start). Empty means the sandbox root itself."),
		message.ToolProperty("tty", "boolean",
			"Request a pseudo-terminal (start). TTY sessions merge stdout/stderr into one stream and support resize."),
		message.ToolProperty("rows", "integer",
			"Pty rows (start / resize). Positive; defaults to 24."),
		message.ToolProperty("cols", "integer",
			"Pty columns (start / resize). Positive; defaults to 80."),
		message.ToolProperty("timeout_seconds", "number",
			"Session-lifetime timeout in seconds (start). Falls back to the tool's default when omitted; zero or negative disables the sandbox-imposed timeout."),
		message.ToolProperty("after_seq", "integer",
			"Read cursor: return output after this sequence number (read). Omit to read from the beginning."),
		message.ToolProperty("max_bytes", "integer",
			"Maximum bytes to return in one read. Defaults to 4096, capped at 1048576."),
		message.ToolProperty("data", "string",
			"Input bytes to write to the process (write)."),
	).Required("action").DisallowAdditionalProperties().Build()
}

// Metadata implements [tool.ToolMetadata]. Sessions execute arbitrary
// sandboxed commands, so the tool mutates state.
func (t *SessionTool) Metadata() tool.ToolMeta {
	return tool.ToolMeta{MutatesState: true}
}

// sessionArgs is the wire-side input. Pointer fields distinguish
// "omitted" from "zero value" where that matters.
type sessionArgs struct {
	Action         string   `json:"action"`
	SessionID      string   `json:"session_id"`
	Command        string   `json:"command"`
	Args           []string `json:"args"`
	Workdir        string   `json:"workdir"`
	TTY            bool     `json:"tty"`
	Rows           int      `json:"rows"`
	Cols           int      `json:"cols"`
	TimeoutSeconds *float64 `json:"timeout_seconds"`
	AfterSeq       *int64   `json:"after_seq"`
	MaxBytes       *int     `json:"max_bytes"`
	Data           string   `json:"data"`
}

// Execute implements [tool.Tool]. It parses the action, dispatches to
// the session manager, and JSON-encodes the result. Sandbox and
// session errors are forwarded with their errdefs classification so
// callers can react without parsing strings.
func (t *SessionTool) Execute(ctx context.Context, arguments string) (string, error) {
	var a sessionArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", errdefs.Validationf("exec_session: parse arguments: %v", err)
	}

	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return "", errdefs.NotAvailablef("exec_session: tool is closed")
	}

	switch a.Action {
	case "start", "":
		return t.start(ctx, a)
	case "read":
		return t.read(ctx, a)
	case "write":
		return t.write(ctx, a)
	case "resize":
		return t.resize(ctx, a)
	case "status":
		return t.status(ctx, a)
	case "terminate":
		return t.terminate(ctx, a)
	case "close":
		return t.close(ctx, a)
	default:
		return "", errdefs.Validationf(
			"exec_session: unknown action %q (start|read|write|resize|status|terminate|close)", a.Action)
	}
}

func (t *SessionTool) start(ctx context.Context, a sessionArgs) (string, error) {
	if strings.TrimSpace(a.Command) == "" {
		return "", errdefs.Validationf("exec_session: command must be non-empty")
	}
	argv := append([]string{a.Command}, a.Args...)
	proc, err := t.pm.Start(ctx, sandbox.ProcessSpec{
		Argv: argv,
		TTY:  a.TTY,
		Rows: a.Rows,
		Cols: a.Cols,
		Opts: sandbox.ExecOptions{
			WorkDir: a.Workdir,
			Timeout: t.resolveSessionTimeout(a.TimeoutSeconds),
		},
	})
	if err != nil {
		return "", err
	}

	t.mu.Lock()
	t.sessions[proc.ID()] = &sessionState{proc: proc, lastUsed: time.Now()}
	t.mu.Unlock()

	payload, err := json.Marshal(struct {
		SessionID string `json:"session_id"`
		PID       int    `json:"pid"`
		TTY       bool   `json:"tty"`
	}{
		SessionID: proc.ID(),
		PID:       proc.PID(),
		TTY:       a.TTY,
	})
	if err != nil {
		return "", errdefs.Internalf("exec_session: encode start result: %v", err)
	}
	return string(payload), nil
}

func (t *SessionTool) read(ctx context.Context, a sessionArgs) (string, error) {
	s, err := t.lookup(a.SessionID)
	if err != nil {
		return "", err
	}
	var afterSeq int64
	if a.AfterSeq != nil {
		afterSeq = *a.AfterSeq
	}
	maxBytes := defaultMaxBytes
	if a.MaxBytes != nil {
		maxBytes = *a.MaxBytes
	}
	if maxBytes <= 0 {
		return "", errdefs.Validationf("exec_session: max_bytes must be positive")
	}
	if maxBytes > maxReadBytes {
		maxBytes = maxReadBytes
	}

	out, err := s.proc.Read(ctx, afterSeq, maxBytes)
	if err != nil {
		return "", classifySessionError(err)
	}
	chunks := make([]chunkResult, 0, len(out.Chunks))
	for _, ch := range out.Chunks {
		chunks = append(chunks, chunkResult{
			Seq:    ch.Seq,
			Stream: ch.Stream.String(),
			Data:   string(ch.Data),
		})
	}
	payload, err := json.Marshal(struct {
		NextSeq int64         `json:"next_seq"`
		EOF     bool          `json:"eof"`
		Chunks  []chunkResult `json:"chunks"`
	}{
		NextSeq: out.NextSeq,
		EOF:     out.EOF,
		Chunks:  chunks,
	})
	if err != nil {
		return "", errdefs.Internalf("exec_session: encode read result: %v", err)
	}
	return string(payload), nil
}

func (t *SessionTool) write(ctx context.Context, a sessionArgs) (string, error) {
	s, err := t.lookup(a.SessionID)
	if err != nil {
		return "", err
	}
	if a.Data == "" {
		return "", errdefs.Validationf("exec_session: data must not be empty")
	}
	if err := s.proc.Write(ctx, []byte(a.Data)); err != nil {
		return "", classifySessionError(err)
	}
	return "{}", nil
}

func (t *SessionTool) resize(ctx context.Context, a sessionArgs) (string, error) {
	s, err := t.lookup(a.SessionID)
	if err != nil {
		return "", err
	}
	if a.Rows <= 0 || a.Cols <= 0 {
		return "", errdefs.Validationf("exec_session: rows and cols must be positive")
	}
	if err := s.proc.Resize(ctx, a.Rows, a.Cols); err != nil {
		return "", classifySessionError(err)
	}
	return "{}", nil
}

func (t *SessionTool) status(ctx context.Context, a sessionArgs) (string, error) {
	if _, err := t.lookup(a.SessionID); err != nil {
		return "", err
	}
	infos, err := t.pm.List(ctx)
	if err != nil {
		return "", err
	}
	for _, info := range infos {
		if info.ID != a.SessionID {
			continue
		}
		out := struct {
			Running  bool     `json:"running"`
			ExitCode int      `json:"exit_code"`
			Reason   string   `json:"reason"`
			Signal   int      `json:"signal"`
			PID      int      `json:"pid"`
			TTY      bool     `json:"tty"`
			Argv     []string `json:"argv"`
		}{
			Running: info.Running,
			PID:     info.PID,
			TTY:     info.TTY,
			Argv:    info.Argv,
		}
		if info.Exit != nil {
			out.ExitCode = info.Exit.Code
			out.Reason = info.Exit.Reason.String()
			out.Signal = info.Exit.Signal
		}
		payload, err := json.Marshal(out)
		if err != nil {
			return "", errdefs.Internalf("exec_session: encode status result: %v", err)
		}
		return string(payload), nil
	}
	return "", errdefs.NotFoundf("exec_session: session %q not found in manager", a.SessionID)
}

func (t *SessionTool) terminate(ctx context.Context, a sessionArgs) (string, error) {
	if _, err := t.lookup(a.SessionID); err != nil {
		return "", err
	}
	// Go through the manager so the exited state is recorded
	// synchronously and status immediately reflects termination.
	if err := t.pm.Terminate(ctx, a.SessionID); err != nil {
		return "", err
	}
	return "{}", nil
}

func (t *SessionTool) close(ctx context.Context, a sessionArgs) (string, error) {
	s, err := t.lookup(a.SessionID)
	if err != nil {
		return "", err
	}
	closeErr := s.proc.Close()
	t.mu.Lock()
	delete(t.sessions, a.SessionID)
	t.mu.Unlock()
	if closeErr != nil {
		return "", classifySessionError(closeErr)
	}
	return "{}", nil
}

func (t *SessionTool) lookup(id string) (*sessionState, error) {
	if id == "" {
		return nil, errdefs.Validationf("exec_session: session_id is required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sessions[id]
	if s == nil {
		return nil, errdefs.NotFoundf("exec_session: unknown session %q", id)
	}
	s.lastUsed = time.Now()
	return s, nil
}

// resolveSessionTimeout maps the optional timeout_seconds knob to a
// time.Duration, mirroring [Tool.resolveTimeout].
func (t *SessionTool) resolveSessionTimeout(s *float64) time.Duration {
	if s == nil {
		return t.defaultTimeout
	}
	if *s <= 0 {
		return 0
	}
	return time.Duration(*s * float64(time.Second))
}

// classifySessionError maps session-level sentinels onto errdefs
// categories so callers can handle them with errdefs.Is*.
func classifySessionError(err error) error {
	switch {
	case errors.Is(err, sandbox.ErrProcessClosed):
		return errdefs.NotFoundf("exec_session: session is closed")
	case errors.Is(err, sandbox.ErrSequenceGap):
		return errdefs.Validationf("exec_session: after_seq points into output the buffer already trimmed; restart from a newer cursor")
	default:
		return err
	}
}

func (t *SessionTool) reapLoop() {
	defer t.wg.Done()
	interval := t.ttl / 4
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.expire()
		}
	}
}

func (t *SessionTool) expire() {
	now := time.Now()
	t.mu.Lock()
	var stale []*sessionState
	for id, s := range t.sessions {
		if now.Sub(s.lastUsed) >= t.ttl {
			delete(t.sessions, id)
			stale = append(stale, s)
		}
	}
	t.mu.Unlock()
	for _, s := range stale {
		_ = s.proc.Close()
	}
}

type chunkResult struct {
	Seq    int64  `json:"seq"`
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

// Compile-time assertion the session tool satisfies the contracts.
// Keeps signature drift in sdk/tool from silently breaking it.
var _ tool.Tool = (*SessionTool)(nil)
