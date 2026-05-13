package exec

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// Name is the canonical tool id callers register and LLMs invoke.
// Stable across versions so prompts referring to the tool by name
// keep working.
const Name = "exec"

// Tool is the LLM-callable shell command runner. It is stateless
// apart from its injected dependencies; safe to register once and
// share across runs.
type Tool struct {
	rn             sandbox.Runner
	defaultTimeout time.Duration
}

// Option configures a [Tool] at construction time.
type Option func(*Tool)

// WithDefaultTimeout sets the per-call timeout used when the LLM
// does not supply timeout_seconds. Zero means "no tool-imposed
// default" — the caller's ctx still applies. Negative values are
// treated as zero.
func WithDefaultTimeout(d time.Duration) Option {
	return func(t *Tool) {
		if d > 0 {
			t.defaultTimeout = d
		}
	}
}

// New constructs the exec tool. rn MUST be non-nil — there is no
// host-shell fallback. Callers that want a literal "always
// succeed" stub for tests should pass [sandbox.NoopRunner]{}
// explicitly; callers that want "always reject" should wrap with
// [sandbox.AllowCommands] over an empty whitelist.
func New(rn sandbox.Runner, opts ...Option) (*Tool, error) {
	if rn == nil {
		return nil, errdefs.Validationf(
			"exec: sandbox.Runner is required (deny-by-default); pass sandbox.NoopRunner{} explicitly if you really want a no-op")
	}
	t := &Tool{rn: rn}
	for _, o := range opts {
		o(t)
	}
	return t, nil
}

// MustNew is the panic-on-error variant of [New] for static wiring
// where the runner is known to be non-nil at compile time.
func MustNew(rn sandbox.Runner, opts ...Option) *Tool {
	t, err := New(rn, opts...)
	if err != nil {
		panic(err)
	}
	return t
}

// Definition implements [tool.Tool]. The description is conservative
// on purpose — the model should not treat exec as a free-form
// scratchpad; explicit cwd / timeout knobs steer it toward
// reproducible calls.
func (t *Tool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name: Name,
		Description: "Run a shell command inside the agent's sandbox. " +
			"Returns exit_code, stdout, and stderr as JSON. A non-zero " +
			"exit_code is reported in the result body, not as an error. " +
			"Use this when you need to inspect files, run scripts, " +
			"compile, or invoke CLIs the user expects you to drive.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The program to run (required). Resolved against the sandbox's PATH policy.",
				},
				"args": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Arguments passed verbatim to the program.",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Working directory, relative to the sandbox root. Empty means the sandbox root itself. Absolute paths or .. escapes are rejected.",
				},
				"stdin": map[string]any{
					"type":        "string",
					"description": "Bytes piped to the program's stdin. Omit when the program does not read stdin.",
				},
				"timeout_seconds": map[string]any{
					"type":        "number",
					"description": "Per-call timeout in seconds. Falls back to the tool's default when omitted. Zero or negative disables the tool-level timeout (the caller's ctx still applies).",
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}

// Metadata implements [tool.ToolMetadata]. The tool can mutate any
// state the sandbox grants the child process — file writes, network
// calls, etc. — so we flag it as mutating without claiming a
// specific RateLimit (real throttling belongs to the sandbox or the
// caller's policy, not this adapter).
func (t *Tool) Metadata() tool.ToolMeta {
	return tool.ToolMeta{MutatesState: true}
}

// args is the wire-side input. Pointer types let us distinguish
// "omitted" from "zero value" for the optional knobs.
type args struct {
	Command        string   `json:"command"`
	Args           []string `json:"args"`
	Workdir        string   `json:"workdir"`
	Stdin          string   `json:"stdin"`
	TimeoutSeconds *float64 `json:"timeout_seconds"`
}

// result is the wire-side output. JSON-encoded as the tool result
// string so structured fields (exit_code) survive the round-trip to
// the model without prose parsing.
type result struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// Execute implements [tool.Tool]. It parses the LLM-supplied
// arguments, builds a [sandbox.ExecOptions], delegates to the
// injected runner, and JSON-encodes the result. Sandbox-level
// errors (errdefs.NotAvailable / Forbidden / Timeout / ...) are
// forwarded verbatim so callers can classify them via errdefs.Is*
// without parsing strings.
func (t *Tool) Execute(ctx context.Context, arguments string) (string, error) {
	var a args
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", errdefs.Validationf("exec: parse arguments: %v", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errdefs.Validationf("exec: command must be non-empty")
	}

	opts := sandbox.ExecOptions{
		WorkDir: a.Workdir,
		Stdin:   nil,
		Timeout: t.resolveTimeout(a.TimeoutSeconds),
	}
	if a.Stdin != "" {
		opts.Stdin = []byte(a.Stdin)
	}

	res, err := t.rn.Exec(ctx, a.Command, a.Args, opts)
	if err != nil {
		return "", err
	}

	payload, err := json.Marshal(result{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
	})
	if err != nil {
		return "", errdefs.Internalf("exec: encode result: %v", err)
	}
	return string(payload), nil
}

// resolveTimeout maps the optional timeout_seconds knob to a
// time.Duration. Nil / negative / zero from the LLM falls back to
// the tool's default; both layers off means "no tool-imposed
// timeout" (ctx still bounds the call).
func (t *Tool) resolveTimeout(s *float64) time.Duration {
	if s == nil {
		return t.defaultTimeout
	}
	if *s <= 0 {
		return 0
	}
	return time.Duration(*s * float64(time.Second))
}

// Compile-time assertion the tool satisfies the contract. Keeps
// signature drift in sdk/tool from silently breaking the adapter.
var _ tool.Tool = (*Tool)(nil)
