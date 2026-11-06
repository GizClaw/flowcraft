package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool"
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

// SessionGroup owns the sessions one deployment's tools share.
//
// The handle table lives here rather than in a tool because two tools address
// the same processes: a model starts a session with one and feeds it with the
// other. Splitting the tools that way keeps each schema small — codex does the
// same with exec_command and write_stdin.
type SessionGroup struct {
	runner sandbox.Runner
	// tty says whether the runner serves pseudo-terminals. When false the
	// schema does not offer tty/rows/cols or resize: a parameter that always
	// fails is worse than an absent one.
	tty bool
	// workdirHint is the directory a start defaults to, shown in the schema.
	workdirHint string

	mu       sync.Mutex
	sessions map[string]sandbox.Session
	next     int
}

// SessionOption configures the group's published surface.
type SessionOption func(*SessionGroup)

// WithTTY publishes pseudo-terminal parameters. Enable it only when the runner
// reports TTY support; the tools cannot tell from a request.
func WithTTY(enabled bool) SessionOption {
	return func(g *SessionGroup) { g.tty = enabled }
}

// WithWorkdirHint names the directory a start defaults to.
func WithWorkdirHint(dir string) SessionOption {
	return func(g *SessionGroup) { g.workdirHint = dir }
}

// NewSession builds the group over runner.
func NewSession(runner sandbox.Runner, opts ...SessionOption) (*SessionGroup, error) {
	if runner == nil {
		return nil, errdefs.Validationf("exec: a sandbox runner is required")
	}
	group := &SessionGroup{runner: runner, sessions: make(map[string]sandbox.Session)}
	for _, opt := range opts {
		opt(group)
	}
	return group, nil
}

// Tools returns the lifecycle tool and the stdin tool.
func (g *SessionGroup) Tools() []tool.Tool {
	return []tool.Tool{&sessionTool{group: g}, &writeStdinTool{group: g}}
}

// Close terminates every session the group still holds.
//
// A tool interface has no teardown, so the host must call this when it retires
// the tools; otherwise a session the model forgot outlives the turn that started
// it.
func (g *SessionGroup) Close() error {
	g.mu.Lock()
	sessions := g.sessions
	g.sessions = make(map[string]sandbox.Session)
	g.mu.Unlock()

	var first error
	for _, session := range sessions {
		if err := session.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
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

// handles returns the ids this group owns.
//
// Ownership is the group's own table, not the runner's: the runner is shared by
// every tool in the deployment, and a model must not be shown — or able to
// address — a session another tool started.
func (g *SessionGroup) handles() map[string]struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make(map[string]struct{}, len(g.sessions))
	for id := range g.sessions {
		ids[id] = struct{}{}
	}
	return ids
}

func (g *SessionGroup) get(id string) (sandbox.Session, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	session, found := g.sessions[id]
	if !found {
		return nil, errdefs.Validationf("%s: no session %q; start one first", SessionName, id)
	}
	return session, nil
}

func (g *SessionGroup) start(ctx context.Context, a sessionArgs) (any, error) {
	if len(a.Argv) == 0 {
		return nil, errdefs.Validationf("%s: argv is required to start", SessionName)
	}
	if a.TTY && !g.tty {
		return nil, errdefs.Validationf(
			"%s: this runner does not serve pseudo-terminals; start without tty", SessionName)
	}
	g.mu.Lock()
	if len(g.sessions) >= maxSessions {
		count := len(g.sessions)
		g.mu.Unlock()
		return nil, errdefs.Validationf("%s: %d sessions are already open, close one first", SessionName, count)
	}
	id := a.ProcessID
	if id == "" {
		g.next++
		id = fmt.Sprintf("s%d", g.next)
	}
	g.mu.Unlock()

	session, err := g.runner.Start(ctx, sandbox.SessionSpec{
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

	g.mu.Lock()
	g.sessions[id] = session
	g.mu.Unlock()
	return map[string]any{"process_id": id, "pid": session.PID()}, nil
}

func (g *SessionGroup) read(ctx context.Context, a sessionArgs) (any, error) {
	session, err := g.get(a.ProcessID)
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
			"description": "Working directory (start); " + workdirDescription(t.group.workdirHint)},
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

func workdirDescription(hint string) string {
	if hint == "" {
		return "defaults to the sandbox root."
	}
	return "defaults to " + hint + "."
}

func (t *sessionTool) Metadata() tool.ToolMeta { return tool.ToolMeta{MutatesState: true} }

func (t *sessionTool) Execute(ctx context.Context, arguments string) (message.Content, error) {
	var a sessionArgs
	if err := tool.DecodeArgs(SessionName, arguments, nil, &a); err != nil {
		return message.Content{}, err
	}
	var (
		payload any
		err     error
	)
	switch a.Action {
	case "start":
		payload, err = t.group.start(ctx, a)
	case "wait":
		payload, err = t.wait(ctx, a)
	case "signal":
		payload, err = t.signal(ctx, a)
	case "terminate":
		payload, err = t.with(a, func(s sandbox.Session) (any, error) {
			return ok(), s.Terminate(ctx)
		})
	case "resize":
		if !t.group.tty {
			return message.Content{}, errdefs.Validationf(
				"%s: this runner does not serve pseudo-terminals", SessionName)
		}
		payload, err = t.with(a, func(s sandbox.Session) (any, error) {
			return ok(), s.Resize(ctx, a.Rows, a.Cols)
		})
	case "close":
		payload, err = t.close(a)
	case "list":
		payload, err = t.list(ctx)
	default:
		err = errdefs.Validationf("%s: unknown action %q", SessionName, a.Action)
	}
	if err != nil {
		return message.Content{}, err
	}
	return encode(SessionName, payload)
}

func (t *sessionTool) wait(ctx context.Context, a sessionArgs) (any, error) {
	session, err := t.group.get(a.ProcessID)
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
func (t *sessionTool) signal(ctx context.Context, a sessionArgs) (any, error) {
	session, err := t.group.get(a.ProcessID)
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

func (t *sessionTool) close(a sessionArgs) (any, error) {
	session, err := t.group.get(a.ProcessID)
	if err != nil {
		return nil, err
	}
	t.group.mu.Lock()
	delete(t.group.sessions, a.ProcessID)
	t.group.mu.Unlock()
	return ok(), session.Close()
}

func (t *sessionTool) list(ctx context.Context) (any, error) {
	infos, err := t.group.runner.List(ctx)
	if err != nil {
		return nil, err
	}
	owned := t.group.handles()
	out := make([]map[string]any, 0, len(owned))
	for _, info := range infos {
		if _, mine := owned[info.ID]; !mine {
			continue
		}
		entry := map[string]any{
			"process_id": info.ID,
			"argv":       info.Argv,
			"tty":        info.TTY,
			"pid":        info.PID,
			"running":    info.Running,
		}
		if info.Exit != nil {
			entry["exit_code"] = info.Exit.Code
		}
		out = append(out, entry)
	}
	return out, nil
}

func (t *sessionTool) with(a sessionArgs, fn func(sandbox.Session) (any, error)) (any, error) {
	session, err := t.group.get(a.ProcessID)
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
