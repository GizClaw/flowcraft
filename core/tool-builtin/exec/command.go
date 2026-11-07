// Package exec implements the built-in command tool: run one command line in the
// sandbox and return its output.
//
// The tool is a thin layer over sandbox.engine.Exec. It decides only what to
// report; confinement, approval and escalation belong to the engine and the
// backend. Its name is the shell it runs (bash, sh, pwsh, cmd), because a model
// reads "bash" more reliably than a generic "exec".
package exec

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
	"github.com/GizClaw/flowcraft/core/tool"
)

const (
	// maxCapturedOutput is what the backend keeps: more than the tool returns, so
	// the tail can be spooled to a file instead of being lost.
	maxCapturedOutput = 8 << 20
	// maxReturnedOutput is what the model sees inline; the rest goes to a file
	// whose path is part of the result.
	maxReturnedOutput = 1 << 20
	// maxReturnedStderr is stderr's share of that budget. A command that writes
	// megabytes to stderr must not push the answer past the inline limit.
	maxReturnedStderr = 256 << 10
)

// CommandTool runs one command line per call through one shell.
type CommandTool struct {
	runner sandbox.Runner
	shell  sandbox.ShellSpec
	// workDir is the default directory; a call may still override it.
	workDir string
	// net is the outbound posture for every call this tool makes.
	net permission.NetPolicy
}

var _ tool.Tool = (*CommandTool)(nil)

// Option configures the tool.
type Option func(*CommandTool)

// WithShell overrides the platform shell; its name becomes the tool name.
func WithShell(spec sandbox.ShellSpec) Option {
	return func(t *CommandTool) { t.shell = spec }
}

// WithNet sets the outbound posture applied to every call. A zero policy keeps
// the backend's construction-time default.
func WithNet(policy permission.NetPolicy) Option {
	return func(t *CommandTool) { t.net = policy }
}

// WithWorkDir sets the directory a command runs in when the call names none.
func WithWorkDir(dir string) Option {
	return func(t *CommandTool) { t.workDir = dir }
}

// NewCommand builds the tool over runner.
func NewCommand(runner sandbox.Runner, opts ...Option) (*CommandTool, error) {
	if runner == nil {
		return nil, errdefs.Validationf("exec: a sandbox runner is required")
	}
	t := &CommandTool{runner: runner, shell: sandbox.DefaultShell(runtime.GOOS)}
	for _, opt := range opts {
		opt(t)
	}
	return t, nil
}

func (t *CommandTool) Definition() message.ToolDefinition {
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Shell command line to run.",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Timeout in seconds; omit for no tool-imposed deadline.",
			},
			"workdir": map[string]any{
				"type":        "string",
				"description": "Absolute directory to run in; " + t.workDirHint(),
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	})
	if err != nil {
		panic("exec: schema: " + err.Error())
	}
	return message.ToolDefinition{
		Name: t.shell.Name,
		Description: "Execute " + t.shell.Name + " commands. " +
			"Returns JSON {exit_code, output, stderr, truncated, full_output_path?}; " +
			"output is stdout and stderr interleaved, and a non-zero exit_code is a " +
			"result, not an error. " +
			"The call blocks until the command exits: run a long job as one call and " +
			"bound it with timeout, rather than starting it repeatedly and polling. " +
			"There is no terminal: stdin is empty, so give every search a path — " +
			"\"rg PATTERN\" with no path reads stdin and reports no matches while the " +
			"tree is full of them. " +
			"Output past " + humanBytes(maxReturnedOutput) + " is written to a file and " +
			"named in full_output_path. " +
			"Use exec_session for interactive or background processes.",
		InputSchema: schema,
	}
}

func (t *CommandTool) workDirHint() string {
	if t.workDir == "" {
		return "defaults to the sandbox root."
	}
	return "defaults to " + t.workDir + "."
}

// Metadata marks the tool as mutating: a command may write.
func (t *CommandTool) Metadata() tool.ToolMeta { return tool.ToolMeta{MutatesState: true} }

func (t *CommandTool) Execute(ctx context.Context, arguments string) (message.Content, error) {
	var params struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
		WorkDir string `json:"workdir"`
	}
	if err := tool.DecodeArgs(t.shell.Name, arguments, map[string]string{"cmd": "command"}, &params); err != nil {
		return message.Content{}, err
	}
	if strings.TrimSpace(params.Command) == "" {
		return message.Content{}, errdefs.Validationf("%s: command is required", t.shell.Name)
	}

	workDir := params.WorkDir
	if workDir == "" {
		workDir = t.workDir
	}
	options := sandbox.ExecOptions{
		WorkDir: workDir,
		Net:     t.net,
		// Capture more than we return: the tail is recoverable only if the
		// backend kept it.
		Resources: sandbox.ResourceLimits{MaxOutputBytes: maxCapturedOutput},
	}
	if params.Timeout > 0 {
		options.Timeout = time.Duration(params.Timeout) * time.Second
	}

	// Always through the shell: the model is writing a command line, and a
	// sometimes-direct spawn interprets the same text two different ways.
	result, err := engine.Exec(ctx, t.runner, t.shell.Path, t.shell.Argv(params.Command), options)
	if err != nil {
		return message.Content{}, err
	}

	// Two streams share one inline budget. Concatenating them first, as this used
	// to, copied up to 8 MiB of stdout and 8 MiB of stderr into a new string, then
	// sliced it, then wrote another copy to the spool file.
	stdout, stdoutCut := truncateOutput(result.Stdout, maxReturnedOutput)
	stderrBudget := maxReturnedOutput - len(stdout)
	if stderrBudget > maxReturnedStderr {
		stderrBudget = maxReturnedStderr
	}
	stderr, stderrCut := truncateOutput(result.Stderr, stderrBudget)
	truncated := stdoutCut || stderrCut

	answer := map[string]any{
		"exit_code": result.ExitCode,
		"output":    stdout,
		"stderr":    stderr,
		"truncated": truncated,
	}
	if truncated {
		// The tool already holds the whole capture, so the rest goes to a file
		// rather than being lost. Spooling to the system temp dir keeps the
		// workspace clean and needs no write permission from the agent's policy:
		// this file is the tool's own artifact, not agent data.
		if path, spoolErr := spool(result.Stdout, result.Stderr); spoolErr == nil {
			answer["full_output_path"] = path
		}
	}
	return encode(t.shell.Name, answer)
}

// spool writes the captured streams to a temp file and returns its path. The
// streams are written as they are: no joined copy is built in memory.
func spool(stdout, stderr string) (string, error) {
	file, err := os.CreateTemp("", "exec-output-*.log")
	if err != nil {
		return "", err
	}
	name := file.Name()
	fail := func(err error) (string, error) {
		_ = file.Close()
		_ = os.Remove(name)
		return "", err
	}
	if stdout != "" {
		if _, err := io.WriteString(file, stdout); err != nil {
			return fail(err)
		}
	}
	if stderr != "" {
		if stdout != "" && !strings.HasSuffix(stdout, "\n") {
			if _, err := io.WriteString(file, "\n"); err != nil {
				return fail(err)
			}
		}
		if _, err := io.WriteString(file, stderr); err != nil {
			return fail(err)
		}
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// truncateOutput cuts s to at most limit bytes, without copying when it fits.
func truncateOutput(s string, limit int) (string, bool) {
	if limit < 0 {
		limit = 0
	}
	if len(s) <= limit {
		return s, false
	}
	return s[:limit], true
}

func humanBytes(n int) string {
	if n >= 1<<20 {
		return strconv.Itoa(n>>20) + " MiB"
	}
	return strconv.Itoa(n>>10) + " KiB"
}

// definition builds a closed object schema for a tool that does not need the
// per-wiring assembly command.go does.
func definition(name, description string, properties map[string]any, required ...string) message.ToolDefinition {
	schema, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	})
	if err != nil {
		panic("exec: schema for " + name + ": " + err.Error())
	}
	return message.ToolDefinition{Name: name, Description: description, InputSchema: schema}
}
