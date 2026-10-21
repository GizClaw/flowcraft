package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/craft/plugin/protocol"
	"github.com/GizClaw/flowcraft/craft/plugin/server"
)

// maxToolName bounds one tool name a plugin may declare. Long enough for a
// namespaced name, short enough that a declaration cannot be a memory
// amplifier.
const maxToolName = 128

// callTimeout bounds one tool call when the caller set no deadline of its
// own. It exists so a plugin that never answers cannot hold a slot forever;
// a caller that wants longer says so with its context.
const callTimeout = 5 * time.Minute

// wireSource is one plugin process's tool contributor: the tools it declares
// over the connection, projected into whatever registry attached.
//
// It replaces the MCP child source: the same Source contract, the same
// Attach/Detach discipline, but the transport underneath is the socket craft
// opened for this process.
type wireSource struct {
	conn   *server.Conn
	id     string
	prefix string
	// declares is the entry's tools grant: this process may publish the
	// tools it declares. A plugin without it still runs — it may publish
	// skills, hooks or nodes — and its declaration is then read and not
	// published.
	declares bool
	// stopped is called once when the process behind this source exits on
	// its own. It is how the host learns a plugin gave up; a source the
	// host closed itself never calls it.
	stopped func()

	mu          sync.Mutex
	registrar   tool.Registrar
	tools       map[string]tool.Tool
	unsubscribe func()
	closed      bool
	cmd         *exec.Cmd
	exited      chan struct{}
	exitErr     error
}

func newWireSource(
	id string,
	conn *server.Conn,
	declares bool,
	stopped func(),
) *wireSource {
	return &wireSource{
		conn:     conn,
		id:       id,
		prefix:   ToolPrefix(id),
		declares: declares,
		stopped:  stopped,
		tools:    make(map[string]tool.Tool),
	}
}

// start spawns the plugin process with the socket craft is listening on.
// The process is craft's child for its whole life: there is no restart,
// because a plugin that died is a plugin whose tools are gone.
func (s *wireSource) start(
	ctx context.Context,
	entry Entry,
	hostEnv map[string]string,
	dataDir string,
) error {
	run := entry.Manifest.Run
	if run == nil {
		return errdefs.Validationf(
			"plugin %s: no run section to start", entry.ID)
	}
	command, args, err := ResolveCommand(entry.Dir, run.Command, run.Args)
	if err != nil {
		return err
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = entry.Dir
	cmd.Env = childEnv(hostEnv, run.Env, entry.ID, dataDir, s.conn.Path())
	// The plugin's output is craft's output, prefixed: a process whose
	// writes go nowhere is a process whose failures cannot be read.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errdefs.Internal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return errdefs.Internal(err)
	}
	if err := cmd.Start(); err != nil {
		return errdefs.Validationf(
			"plugin %s: start %s: %w", entry.ID, command, err)
	}
	s.mu.Lock()
	s.cmd = cmd
	s.exited = make(chan struct{})
	exited := s.exited
	s.mu.Unlock()
	pump(ctx, entry.ID, "stdout", stdout)
	pump(ctx, entry.ID, "stderr", stderr)
	go s.reap(cmd, exited)
	return nil
}

// reap waits for the process and reports a loss the host did not ask for.
func (s *wireSource) reap(cmd *exec.Cmd, exited chan struct{}) {
	err := cmd.Wait()
	s.mu.Lock()
	if s.exitErr == nil {
		s.exitErr = err
	}
	closed := s.closed
	s.mu.Unlock()
	close(exited)
	if closed {
		return
	}
	// The socket craft is listening on outlives the process, so the
	// connection has to be failed explicitly: nothing else will, and a
	// wait on it would run to its timeout for a plugin that is gone.
	s.conn.Fail(errdefs.NotAvailablef(
		"plugin %s: the process exited: %s", s.id, exitDescription(err)))
	if s.stopped != nil {
		s.stopped()
	}
}

func exitDescription(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// pump forwards one output stream of the plugin to the host log, one line at
// a time, attributed to the plugin that wrote it.
func pump(ctx context.Context, id, stream string, reader interface{ Read([]byte) (int, error) }) {
	go func() {
		buffer := make([]byte, 0, 4096)
		chunk := make([]byte, 4096)
		for {
			n, err := reader.Read(chunk)
			if n > 0 {
				buffer = append(buffer, chunk[:n]...)
				for {
					index := indexByte(buffer, '\n')
					if index < 0 {
						break
					}
					line := strings.TrimRight(string(buffer[:index]), "\r")
					if line != "" {
						telemetry.Info(ctx, "plugin output: "+line,
							attribute.String("plugin", id),
							attribute.String("stream", stream))
					}
					buffer = buffer[index+1:]
				}
			}
			if err != nil {
				if len(buffer) > 0 {
					telemetry.Info(ctx, "plugin output: "+string(buffer),
						attribute.String("plugin", id),
						attribute.String("stream", stream))
				}
				return
			}
		}
	}()
}

func indexByte(buffer []byte, target byte) int {
	for i, value := range buffer {
		if value == target {
			return i
		}
	}
	return -1
}

// childEnv is the environment of one plugin process: the host's, then the
// manifest's, then the three facts craft states and a manifest cannot
// override. The process inherits the environment craft was started with —
// a plugin that is a command run through an interpreter ("go run ./server")
// needs the same PATH and toolchain settings craft's own process has — and
// a key stated here replaces the inherited one rather than adding a second
// copy of it.
func childEnv(
	hostEnv map[string]string,
	manifestEnv map[string]string,
	id string,
	dataDir string,
	wire string,
) []string {
	merged := make(map[string]string, len(hostEnv)+len(manifestEnv)+3)
	for key, value := range hostEnv {
		merged[key] = value
	}
	for key, value := range manifestEnv {
		merged[key] = value
	}
	merged[protocol.IDEnv] = id
	merged[protocol.DataDirEnv] = dataDir
	merged[protocol.WireEnv] = wire
	inherited := os.Environ()
	out := make([]string, 0, len(inherited)+len(merged))
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, replaced := merged[key]; replaced {
			continue
		}
		out = append(out, entry)
	}
	for key, value := range merged {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

// Tools implements [Source].
func (s *wireSource) Tools() []tool.Tool {
	s.mu.Lock()
	out := make([]tool.Tool, 0, len(s.tools))
	for _, candidate := range s.tools {
		out = append(out, candidate)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].Definition().Name < out[j].Definition().Name
	})
	return out
}

// LazyTools implements [Source]. A plugin declares what it wants published;
// deferring a schema is the plugin's decision to make, not craft's.
func (s *wireSource) LazyTools() []tool.LazyTool { return nil }

// Attach implements [tool.RegistryAttacher]: the registry attaches once and
// receives whatever the plugin has declared, plus every later declaration.
func (s *wireSource) Attach(registrar tool.Registrar) {
	if registrar == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.registrar = registrar
	s.mu.Unlock()
	if s.unsubscribe == nil {
		unsubscribe := s.conn.OnDeclare(s.project)
		s.mu.Lock()
		s.unsubscribe = unsubscribe
		s.mu.Unlock()
	}
	declaration, _ := s.conn.Declaration()
	if declaration != nil {
		s.project(declaration)
	}
}

// Detach implements [tool.RegistryDetacher].
func (s *wireSource) Detach(tool.Registrar) {
	s.unsubscribeNow()
	s.mu.Lock()
	s.registrar = nil
	s.mu.Unlock()
}

func (s *wireSource) unsubscribeNow() {
	s.mu.Lock()
	unsubscribe := s.unsubscribe
	s.unsubscribe = nil
	s.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
}

// project publishes one declaration: the tools it names are added, the ones
// it dropped are removed.
func (s *wireSource) project(declaration *protocol.Declaration) {
	if declaration == nil || !s.declares {
		return
	}
	desired := make(map[string]tool.Tool, len(declaration.GetTools()))
	for _, declared := range declaration.GetTools() {
		name := s.prefix + declared.GetName()
		desired[name] = &wireTool{
			source: s,
			name:   name,
			local:  declared.GetName(),
			def: message.ToolDefinition{
				Name:        name,
				Description: declared.GetDescription(),
				InputSchema: json.RawMessage(declared.GetInputSchema()),
			},
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	registrar := s.registrar
	var added []tool.Tool
	var removed []string
	for name, candidate := range desired {
		if _, known := s.tools[name]; !known {
			added = append(added, candidate)
		}
	}
	for name := range s.tools {
		if _, wanted := desired[name]; !wanted {
			removed = append(removed, name)
		}
	}
	s.tools = desired
	s.mu.Unlock()
	sort.Slice(added, func(i, j int) bool {
		return added[i].Definition().Name < added[j].Definition().Name
	})
	sort.Strings(removed)
	if registrar == nil {
		return
	}
	for _, candidate := range added {
		if err := registrar.Add(candidate); err != nil {
			telemetry.WarnErr(context.Background(),
				"plugin host: cannot publish a plugin tool", err,
				attribute.String("plugin", s.id),
				attribute.String("tool", candidate.Definition().Name))
		}
	}
	for _, name := range removed {
		registrar.Remove(name)
	}
}

// CallTool invokes one of the plugin's tools by its local name. Plugin graph
// nodes address tools without the namespace, which is what the manifest's
// nodes[].tool carries.
func (s *wireSource) CallTool(
	ctx context.Context,
	toolName string,
	args any,
) (json.RawMessage, error) {
	if !s.declares {
		return nil, errdefs.Forbiddenf(
			"plugin %s: tools are ignored without the %s grant", s.id, GrantTools)
	}
	if !s.hasTool(toolName) {
		return nil, errdefs.NotFoundf(
			"plugin %s: no tool named %q", s.id, toolName)
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return nil, errdefs.Validationf(
			"plugin %s: tool %q arguments: %w", s.id, toolName, err)
	}
	timeout := callTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 {
			timeout = remaining
		}
	}
	result, err := s.conn.CallTool(ctx, toolName, payload, timeout)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 || !json.Valid(result) {
		// A result that is not JSON is still a result: it crosses as a
		// JSON string so the caller always sees valid JSON.
		encoded, marshalErr := json.Marshal(string(result))
		if marshalErr != nil {
			return nil, errdefs.Internal(marshalErr)
		}
		return encoded, nil
	}
	return json.RawMessage(result), nil
}

func (s *wireSource) hasTool(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for published := range s.tools {
		if published == s.prefix+name {
			return true
		}
	}
	return false
}

// WaitReady implements [Readiness]: the plugin is up when its wire handshake
// finished, which is the only moment craft may dispatch work to it.
func (s *wireSource) WaitReady(ctx context.Context, timeout time.Duration) error {
	return s.conn.WaitReady(ctx, timeout)
}

// Close implements [Source]: the connection, then the process.
func (s *wireSource) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cmd := s.cmd
	s.mu.Unlock()
	s.unsubscribeNow()
	var errs []error
	if err := s.conn.Close(); err != nil {
		errs = append(errs, err)
	}
	if cmd != nil && cmd.Process != nil {
		// The process is craft's child and craft asked it to stop. A
		// child that already exited is not a failure.
		if err := signalStop(cmd.Process); err != nil {
			telemetry.WarnErr(context.Background(),
				"plugin host: cannot stop the plugin process", err,
				attribute.String("plugin", s.id))
		}
	}
	return errorsJoin(errs)
}

// wireTool is one declared tool as the tool registry sees it.
type wireTool struct {
	source *wireSource
	name   string
	local  string
	def    message.ToolDefinition
}

func (t *wireTool) Definition() message.ToolDefinition { return t.def }

func (t *wireTool) Execute(
	ctx context.Context,
	arguments string,
) (message.Content, error) {
	if arguments == "" {
		arguments = "{}"
	}
	payload, err := t.source.CallTool(ctx, t.local, json.RawMessage(arguments))
	if err != nil {
		return message.Content{}, err
	}
	return message.NewTextContent(textResult(payload)), nil
}

// textResult is what one plugin answer means as text. A plugin that answers
// with a bare JSON string is answering with text — the content a core text
// tool returns — so the quotes come off; a JSON object or array crosses as
// the JSON it is.
func textResult(payload []byte) string {
	var quoted string
	if len(payload) > 1 && payload[0] == '"' &&
		json.Unmarshal(payload, &quoted) == nil {
		return quoted
	}
	return string(payload)
}

// staticSource is a plugin with no run section: it publishes nothing, and a
// call through it says why instead of reporting an empty result.
type staticSource struct{ id string }

func (s staticSource) Tools() []tool.Tool         { return nil }
func (s staticSource) LazyTools() []tool.LazyTool { return nil }
func (s staticSource) Attach(tool.Registrar)      {}
func (s staticSource) Close() error               { return nil }

func (s staticSource) CallTool(
	context.Context,
	string,
	any,
) (json.RawMessage, error) {
	return nil, errdefs.Forbiddenf(
		"plugin %s: the plugin declares no run section, so it has no tools",
		s.id)
}

// validateDeclaration is what craft checks about a declaration before it
// accepts it. The checks live here rather than in the wire package because
// they are about the tool registry craft is about to publish into.
func validateDeclaration(id string, declaration *protocol.Declaration) error {
	seen := make(map[string]int, len(declaration.GetTools()))
	for index, declared := range declaration.GetTools() {
		name := declared.GetName()
		if strings.TrimSpace(name) == "" {
			return errdefs.Validationf(
				"plugin %s: tools[%d] has no name", id, index)
		}
		if len(name) > maxToolName {
			return errdefs.Validationf(
				"plugin %s: tool name %q is longer than %d characters",
				id, name[:32]+"...", maxToolName)
		}
		if first, duplicate := seen[name]; duplicate {
			return errdefs.Validationf(
				"plugin %s: tools %d and %d are both named %q",
				id, first, index, name)
		}
		seen[name] = index
		if schema := declared.GetInputSchema(); len(schema) == 0 {
			// An empty schema is an object with no properties, not an
			// error: a tool that takes no arguments is a normal tool.
			continue
		}
	}
	return nil
}

// wireSource is the production source factory: one socket per plugin
// process, the process that dials it, and the tools it declares over it.
//
// A plugin with no run section gets a [staticSource] instead: it declares no
// process, so there is nothing to listen for and nothing that will arrive.
func (h *Host) wireSource(
	ctx context.Context,
	entry Entry,
	generation uint64,
	env map[string]string,
	dataDir string,
) (Source, error) {
	if entry.Manifest.Run == nil {
		return staticSource{id: entry.ID}, nil
	}
	if h.server == nil {
		return nil, errdefs.NotAvailablef(
			"plugin host: plugin %s has a run section but no wire server is "+
				"configured", entry.ID)
	}
	projection := entry.Projection()
	conn, err := h.server.NewConn(server.ConnOptions{
		PluginID:   entry.ID,
		Version:    entry.Manifest.Version,
		Generation: generation,
		Grants:     entry.Grants,
		Validate: func(declaration *protocol.Declaration) error {
			return validateDeclaration(entry.ID, declaration)
		},
	})
	if err != nil {
		return nil, err
	}
	source := newWireSource(
		entry.ID,
		conn,
		projection.Tools,
		func() { h.notifyLost(entry.ID) },
	)
	if err := source.start(ctx, entry, env, dataDir); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return source, nil
}

func errorsJoin(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return fmt.Errorf("%w; and %w", errs[0], errs[1])
	}
}
