// Package starlark runs script nodes with Starlark, in process.
//
// Starlark is chosen for the properties a rule wants and a language rarely gives
// for free: it terminates (no while, no recursion), it is deterministic (no
// clock, no randomness, no IO), and it cannot reach the host except through what
// the caller predeclares. That makes it safe enough to run beside the agent
// rather than in a process, and fast enough that a rule node costs microseconds.
//
// What it does not give is a memory bound: the step budget bounds work, not
// allocation. A deployment that needs a hard memory cap must run the agent under
// its own limit or pick a backend that isolates the script.
package starlark

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/errdefs"
	star "go.starlark.net/starlark"
)

// DefaultMaxSteps is the step budget an unconfigured node gets.
//
// A rule script is small; a million steps is far past any honest one and still
// bounds a runaway loop to tens of milliseconds, which is the point of having a
// default at all.
const DefaultMaxSteps = 1_000_000

// entry is the function a script must define.
const entry = "run"

// backend is the in-process Starlark engine.
type backend struct{}

// New returns the Starlark backend.
func New() script.Backend { return backend{} }

// Name implements [script.Backend].
func (backend) Name() string { return "starlark" }

// Capabilities implements [script.Backend].
//
// Deterministic and terminating are language guarantees. MemoryBounded is not
// claimed: the interpreter allocates on the host heap, and a step budget is not a
// memory bound. Isolated is not claimed either: the script runs in this process.
func (backend) Capabilities() script.Capabilities {
	return script.Capabilities{
		Deterministic: true,
		Terminating:   true,
		// A tool call is a Go function call through the host, so a rule can use
		// the tools its document allows.
		Tools: true,
	}
}

// Compile implements [script.Backend].
//
// The program is immutable, so one compiled value serves every concurrent
// invocation; each run gets its own thread and its own globals.
func (backend) Compile(_ context.Context, source script.Script) (script.Program, error) {
	_, program, err := star.SourceProgram(source.Name, source.Text, isPredeclared)
	if err != nil {
		// A syntax error or an undefined name is an authoring mistake, and the
		// message already points at the file and line.
		return nil, errdefs.Validationf("starlark: %s", firstLine(err.Error()))
	}
	return &compiled{
		program: program,
		name:    source.Name,
		version: source.Version,
	}, nil
}

// isPredeclared tells the resolver which free names the host supplies. `print` is
// not listed because Starlark's universe already provides it.
func isPredeclared(name string) bool { return name == "log" || name == "tool" }

// compiled is one resolved program.
type compiled struct {
	program *star.Program
	name    string
	version string
}

// Version implements [script.Program].
func (c *compiled) Version() string { return c.version }

// Run implements [script.Backend].
//
// Top-level code runs on every invocation, which is what keeps one call from
// observing another's globals. The cost is the compiled top level, not a reparse,
// and a script that does its work inside run() pays nothing for it.
func (b backend) Run(
	ctx context.Context,
	program script.Program,
	state map[string]any,
	budget script.Budget,
	host script.Host,
) (map[string]any, error) {
	compiled, ok := program.(*compiled)
	if !ok {
		return nil, errdefs.Internalf("starlark: unexpected program type %T", program)
	}
	thread := &star.Thread{Name: "script:" + compiled.name}
	thread.Print = func(_ *star.Thread, msg string) { host.Log(script.LogInfo, msg) }

	steps := budget.MaxSteps
	if steps == 0 {
		steps = DefaultMaxSteps
	}
	thread.SetMaxExecutionSteps(steps)

	// The two ways one run stops early: the caller gave up, or the step budget
	// ran out. Cancel is goroutine-safe, which is what makes the first reachable.
	var exhausted bool
	thread.OnMaxSteps = func(t *star.Thread) {
		exhausted = true
		t.Cancel("step budget exhausted")
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel(ctx.Err().Error())
		case <-stop:
		}
	}()

	globals, err := compiled.program.Init(thread, star.StringDict{
		"log":  builtinLog(host),
		"tool": builtinTool(ctx, host),
	})
	if err != nil {
		return nil, b.failure(ctx, compiled.name, steps, thread, &exhausted, err)
	}
	fn, ok := globals[entry]
	if !ok {
		return nil, errdefs.Validationf(
			"starlark: %s does not define %s(state)", compiled.name, entry)
	}
	argument, err := toStarlark(state, thread)
	if err != nil {
		return nil, errdefs.Validationf("starlark: %s: state: %w", compiled.name, err)
	}
	result, err := star.Call(thread, fn, star.Tuple{argument}, nil)
	if err != nil {
		return nil, b.failure(ctx, compiled.name, steps, thread, &exhausted, err)
	}
	dict, ok := result.(*star.Dict)
	if !ok {
		return nil, errdefs.Abortedf(
			"starlark: %s returned %s, want a dict of state writes", compiled.name, result.Type())
	}
	writes, err := fromStarlarkDict(dict)
	if err != nil {
		return nil, errdefs.Abortedf("starlark: %s: %w", compiled.name, err)
	}
	return writes, nil
}

// failure classifies one run failure.
//
// A cancellation the caller caused is the caller's error, not the script's; an
// exhausted budget has its own category; everything else is the script failing,
// which the node reports as aborted.
func (b backend) failure(
	ctx context.Context,
	name string,
	steps uint64,
	thread *star.Thread,
	exhausted *bool,
	err error,
) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if *exhausted {
		return errdefs.BudgetExceededf(
			"starlark: %s exhausted its %d step budget", name, steps)
	}
	return fmt.Errorf("starlark: %s: %w", name, err)
}

// builtinLog is the script's `log(*args)`.
//
// It renders like print but reports through the host, so a rule's note lands in
// the deployment's log rather than on a stream nobody reads.
func builtinLog(host script.Host) *star.Builtin {
	return star.NewBuiltin("log", func(_ *star.Thread, _ *star.Builtin, args star.Tuple, kwargs []star.Tuple) (star.Value, error) {
		if len(kwargs) > 0 {
			return nil, fmt.Errorf("log: unexpected keyword argument %s", kwargs[0][0])
		}
		parts := make([]string, 0, len(args))
		for _, arg := range args {
			parts = append(parts, render(arg))
		}
		host.Log(script.LogInfo, strings.Join(parts, " "))
		return star.None, nil
	})
}

// builtinTool is the script's `tool(name, args)`.
//
// The policy lives in the host: this binding only spells the call. A script whose
// config allows no tools gets a refusal the first time it tries, which is a
// clearer failure than a name that does not resolve.
func builtinTool(ctx context.Context, host script.Host) *star.Builtin {
	return star.NewBuiltin("tool", func(thread *star.Thread, _ *star.Builtin, args star.Tuple, kwargs []star.Tuple) (star.Value, error) {
		var name string
		var argument star.Value = star.None
		if err := star.UnpackArgs("tool", args, kwargs, "name", &name, "args?", &argument); err != nil {
			return nil, err
		}
		callArgs := map[string]any{}
		if argument != star.None {
			converted, err := fromStarlark(argument)
			if err != nil {
				return nil, fmt.Errorf("tool: args: %w", err)
			}
			dict, ok := converted.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tool: args must be a dict, got %s", argument.Type())
			}
			callArgs = dict
		}
		result, err := host.Tool(ctx, name, callArgs)
		if err != nil {
			return nil, err
		}
		answer, err := toStarlark(map[string]any{"ok": result.OK, "text": result.Text}, thread)
		if err != nil {
			return nil, fmt.Errorf("tool: result: %w", err)
		}
		return answer, nil
	})
}

// render is how one argument reads in a log line: a string verbatim, anything
// else with its Starlark spelling.
func render(value star.Value) string {
	if text, ok := star.AsString(value); ok {
		return text
	}
	return value.String()
}

// firstLine keeps a compile error to its first line: a Starlark traceback names
// the same file and line once, and the frames above it are the compiler's.
func firstLine(message string) string {
	if before, _, found := strings.Cut(message, "\n"); found {
		return before
	}
	return message
}
