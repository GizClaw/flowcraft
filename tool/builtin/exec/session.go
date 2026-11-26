package exec

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"github.com/goccy/go-json"
)

// Tool names, as the model sees them.
const (
	// SessionName manages a session's life: start, wait, signal, terminate,
	// close, list.
	SessionName = "exec_session"
	// WriteStdinName talks to a running session: send input, read output.
	WriteStdinName = "write_stdin"
)

const (
	maxSessions  = 64
	maxReadBytes = 1 << 20
	defaultRead  = 4096
)

// SessionGroup is the published surface of the session tools.
//
// The handle table is not here: it belongs to one conversation, so it lives in that session's scratch
// space and a model can neither see nor address a process another session started. What is here is
// what depends on the deployment rather than the session — whether the backend serves
// pseudo-terminals — because a schema cannot change between two calls of one tool.
type SessionGroup struct {
	// tty says whether the backend serves pseudo-terminals. When false the schema does not offer
	// tty/rows/cols or resize: a parameter that always fails is worse than an absent one.
	tty bool
}

// tables holds one process table per open conversation.
//
// It cannot live in the call's scratch space: dispatch partitions that per tool name, and these are two
// tools — the lifecycle one and the stdin one — addressing the same processes. The map holds an entry
// only while the conversation has processes open, so it is not a session registry.
var tables sync.Map // session id → *table

// table is one conversation's open processes.
//
// Two tools address the same processes — a model starts a session with one and feeds it with the
// other — which is why the table belongs to the session and not to either tool.
type table struct {
	// session is the conversation this table belongs to, so it can prune itself when the last
	// process is gone without the caller having to remember the key.
	session  string
	mu       sync.Mutex
	sessions map[string]sandbox.Session
	next     int
}

// tableFor returns the process table of one conversation, creating it on first use.
func tableFor(binding tool.Binding) *table {
	id := binding.Session.ID()
	if existing, ok := tables.Load(id); ok {
		return existing.(*table)
	}
	created := &table{session: id, sessions: make(map[string]sandbox.Session)}
	actual, _ := tables.LoadOrStore(id, created)
	return actual.(*table)
}

// forget drops the table once its last process is gone, so the package's map is not a session
// registry that outlives the sessions it describes.
func (t *table) forget() {
	t.mu.Lock()
	remaining := len(t.sessions)
	t.mu.Unlock()
	if remaining == 0 {
		tables.Delete(t.session)
	}
}

// starter reads the session's process surface out of the call.
func starter(binding tool.Binding) (sandbox.SessionStarter, error) {
	surface, ok := platform.ProcessSlot.Get(binding.Caps)
	if !ok || surface == nil {
		return nil, errdefs.NotAvailablef("%s: this session has no process surface", SessionName)
	}
	return surface, nil
}

// SessionOption configures the group's published surface.
type SessionOption func(*SessionGroup)

// WithTTY publishes pseudo-terminal parameters. Enable it only when the runner
// reports TTY support; the tools cannot tell from a request.
func WithTTY(enabled bool) SessionOption {
	return func(g *SessionGroup) { g.tty = enabled }
}

// NewSession builds the group.
func NewSession(opts ...SessionOption) *SessionGroup {
	group := &SessionGroup{}
	for _, opt := range opts {
		opt(group)
	}
	return group
}

// Tools returns the lifecycle tool and the stdin tool.
// LazyTools is empty: the session tools are local, and the processes they start
// are deferred until a session begins.
func (g *SessionGroup) LazyTools() []tool.LazyTool { return nil }

func (g *SessionGroup) Tools() []tool.Tool {
	return []tool.Tool{&sessionTool{group: g}, &writeStdinTool{group: g}}
}

func ok() map[string]any { return map[string]any{"ok": true} }

// sessionArgs is the union of the arguments both tools accept.
type sessionArgs struct {
	Action         string   `json:"action"`
	ProcessID      string   `json:"process_id"`
	Argv           []string `json:"argv"`
	TTY            bool     `json:"tty"`
	Rows           int      `json:"rows"`
	Cols           int      `json:"cols"`
	WorkDir        string   `json:"workdir"`
	AfterSeq       int64    `json:"after_seq"`
	MaxBytes       int      `json:"max_bytes"`
	Data           string   `json:"data"`
	CloseStdin     bool     `json:"close_stdin"`
	Signal         string   `json:"signal"`
	TimeoutSeconds float64  `json:"timeout_seconds"`
}

func (t *table) get(id string) (sandbox.Session, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	session, found := t.sessions[id]
	if !found {
		return nil, errdefs.Validationf("%s: no session %q; start one first", SessionName, id)
	}
	return session, nil
}

func (t *table) start(ctx context.Context, starter sandbox.SessionStarter, published *SessionGroup, a sessionArgs) (any, error) {
	if len(a.Argv) == 0 {
		return nil, errdefs.Validationf("%s: argv is required to start", SessionName)
	}
	if a.TTY && !published.tty {
		// The group publishes this, because a schema cannot change between two calls; a request that
		// asks for something the deployment did not publish is a caller mistake, not a backend one.
		return nil, errdefs.Validationf(
			"%s: pseudo-terminals are not published by this deployment; start without tty", SessionName)
	}
	if strings.TrimSpace(a.WorkDir) == "" {
		return nil, errdefs.Validationf("%s: workdir is required to start", SessionName)
	}
	t.mu.Lock()
	if len(t.sessions) >= maxSessions {
		count := len(t.sessions)
		t.mu.Unlock()
		return nil, errdefs.Validationf("%s: %d sessions are already open, close one first", SessionName, count)
	}
	id := a.ProcessID
	if id == "" {
		t.next++
		id = fmt.Sprintf("s%d", t.next)
	}
	t.mu.Unlock()

	session, err := starter.Start(ctx, sandbox.SessionSpec{
		ID:    id,
		Argv:  a.Argv,
		TTY:   a.TTY,
		Rows:  a.Rows,
		Cols:  a.Cols,
		Stdin: true,
		Opts:  sandbox.ExecOptions{WorkDir: a.WorkDir},
	})
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	t.sessions[id] = session
	t.mu.Unlock()
	return map[string]any{"process_id": id, "pid": session.PID()}, nil
}

func (t *table) read(ctx context.Context, a sessionArgs) (any, error) {
	session, err := t.get(a.ProcessID)
	if err != nil {
		return nil, err
	}
	maxBytes := a.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultRead
	}
	if maxBytes > maxReadBytes {
		maxBytes = maxReadBytes
	}
	output, err := session.Read(ctx, a.AfterSeq, maxBytes)
	if err != nil {
		return nil, err
	}
	chunks := make([]map[string]any, 0, len(output.Chunks))
	for _, chunk := range output.Chunks {
		chunks = append(chunks, map[string]any{
			"seq":    chunk.Seq,
			"stream": chunk.Stream.String(),
			"data":   string(chunk.Data),
		})
	}
	return map[string]any{
		"process_id": a.ProcessID,
		"chunks":     chunks,
		"next_seq":   output.NextSeq,
		"eof":        output.EOF,
	}, nil
}

// sessionTool manages a session's life.
type sessionTool struct{ group *SessionGroup }

var _ tool.Tool = (*sessionTool)(nil)

func (t *sessionTool) Definition() message.ToolDefinition {
	actions := "start | wait | signal | terminate | close | list"
	if t.group.tty {
		actions += " | resize"
	}
	properties := map[string]any{
		"action": map[string]any{"type": "string", "description": actions},
		"process_id": map[string]any{"type": "string",
			"description": "Session id; omit on start to have one assigned."},
		"argv": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
			"description": "Command and arguments (start)."},
		"workdir": map[string]any{"type": "string",
			"description": "Absolute working directory (start); required, because the tool has no directory of its own."},
		"signal": map[string]any{"type": "string",
			"description": "interrupt (default) or kill, which stops the session (signal)."},
		"timeout_seconds": map[string]any{"type": "number", "minimum": 1,
			"description": "Bound how long wait blocks (wait)."},
	}
	if t.group.tty {
		properties["tty"] = map[string]any{"type": "boolean", "description": "Request a pseudo-terminal (start)."}
		properties["rows"] = map[string]any{"type": "integer", "minimum": 1, "description": "PTY rows (start/resize)."}
		properties["cols"] = map[string]any{"type": "integer", "minimum": 1, "description": "PTY columns (start/resize)."}
	}
	return definition(SessionName,
		"Manage a running session: start a command, signal or stop it, wait for its "+
			"exit, close it, or list the sessions this tool started. Output is read with "+
			WriteStdinName+".", properties, "action")
}

func (t *sessionTool) Metadata() tool.ToolMeta { return tool.ToolMeta{MutatesState: true} }

// Needs declares the session's process surface.
func (*sessionTool) Needs() []tool.Need { return []tool.Need{platform.ProcessSlot} }

func (t *sessionTool) Execute(ctx context.Context, binding tool.Binding, arguments string) (message.Content, error) {
	var a sessionArgs
	if err := tool.DecodeArgs(SessionName, arguments, nil, &a); err != nil {
		return message.Content{}, err
	}
	surface, err := starter(binding)
	if err != nil {
		return message.Content{}, err
	}
	processes := tableFor(binding)
	var payload any
	switch a.Action {
	case "start":
		payload, err = processes.start(ctx, surface, t.group, a)
	case "wait":
		payload, err = processes.wait(ctx, a)
	case "signal":
		payload, err = processes.signal(ctx, a)
	case "terminate":
		payload, err = processes.with(a, func(s sandbox.Session) (any, error) {
			return ok(), s.Terminate(ctx)
		})
	case "resize":
		if !t.group.tty {
			return message.Content{}, errdefs.Validationf(
				"%s: this runner does not serve pseudo-terminals", SessionName)
		}
		payload, err = processes.with(a, func(s sandbox.Session) (any, error) {
			return ok(), s.Resize(ctx, a.Rows, a.Cols)
		})
	case "close":
		payload, err = processes.close(a)
	case "list":
		payload, err = processes.list(ctx)
	default:
		err = errdefs.Validationf("%s: unknown action %q", SessionName, a.Action)
	}
	if err != nil {
		return message.Content{}, err
	}
	return encode(SessionName, payload)
}

func (t *table) wait(ctx context.Context, a sessionArgs) (any, error) {
	session, err := t.get(a.ProcessID)
	if err != nil {
		return nil, err
	}
	waitCtx := ctx
	if a.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, time.Duration(a.TimeoutSeconds*float64(time.Second)))
		defer cancel()
	}
	exit, err := session.Wait(waitCtx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"process_id": a.ProcessID,
		"exit_code":  exit.Code,
		"signal":     exit.Signal,
		"reason":     exit.Reason.String(),
	}, nil
}

// signal delivers a soft signal, or stops the session for "kill".
//
// The sandbox contract has one soft signal (interrupt); "kill" is therefore
// Terminate, which the model can also ask for directly. Mapping it here keeps
// the vocabulary a model expects without widening the contract for a synonym.
func (t *table) signal(ctx context.Context, a sessionArgs) (any, error) {
	session, err := t.get(a.ProcessID)
	if err != nil {
		return nil, err
	}
	switch a.Signal {
	case "", "interrupt":
		return ok(), session.Signal(ctx, sandbox.SessionSignalInterrupt)
	case "kill":
		return ok(), session.Terminate(ctx)
	default:
		return nil, errdefs.Validationf("%s: unknown signal %q", SessionName, a.Signal)
	}
}

func (t *table) close(a sessionArgs) (any, error) {
	session, err := t.get(a.ProcessID)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	delete(t.sessions, a.ProcessID)
	t.mu.Unlock()
	t.forget()
	return ok(), session.Close()
}

func (t *table) list(ctx context.Context) (any, error) {
	// The table is the answer: a session may not enumerate the runner, because a starter starts
	// processes and nothing else, and a model must not see a session another conversation opened.
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]map[string]any, 0, len(t.sessions))
	for id, session := range t.sessions {
		out = append(out, map[string]any{
			"process_id": id,
			"pid":        session.PID(),
			"tty":        session.Capabilities().TTY,
		})
	}
	return out, nil
}

func (t *table) with(a sessionArgs, fn func(sandbox.Session) (any, error)) (any, error) {
	session, err := t.get(a.ProcessID)
	if err != nil {
		return nil, err
	}
	return fn(session)
}

// encode marshals one tool answer.
func encode(toolName string, payload any) (message.Content, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return message.Content{}, errdefs.Internalf("%s: encode result: %v", toolName, err)
	}
	return message.NewTextContent(string(encoded)), nil
}
