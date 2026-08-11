package exec

import (
	"context"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
	toolconfig "github.com/GizClaw/flowcraft/sdk/tool/config"
)

// RegisterBuiltin wires the sandbox-backed exec and exec_session tools
// into a tool config builder as builtin factories. Unlike a
// pre-constructed builtin, each factory runs once per Build and pulls
// the sandbox runner out of the tool.Assembly resource's deps, so the
// deployment document owns which sandbox the tools execute under:
//
//	// host wiring
//	exec.RegisterBuiltin(toolBuilder)
//	deployBuilder.MustRegisterResource(toolBuilder)
//
//	// deploy.yaml
//	resources:
//	  boxes:
//	    kind: sandbox.Registry
//	    impl: yaml
//	    settings: {file: ./sandboxes.yaml}
//	  tools:
//	    kind: tool.Assembly
//	    impl: yaml
//	    deps: {sandbox: boxes/main}
//	    settings: {file: ./tools.yaml}
//
//	// tools.yaml
//	version: v1
//	sources:
//	  - kind: builtin
//	    spec: {tools: [exec, exec_session]}
//
// The exec factory fails closed when the sandbox dep is absent or not
// a sandbox.Runner. exec_session additionally requires the runner to
// implement sandbox.ProcessManager (local, bwrap, and seatbelt runners
// all do); a runner without sessions fails with NotAvailable when the
// document names it. Duplicate registration is a programming bug and
// panics inside the builder.
func RegisterBuiltin(b *toolconfig.Builder) {
	b.RegisterBuiltinFactory(Name, execFactory)
	b.RegisterBuiltinFactory(SessionName, sessionFactory)
}

func execFactory(_ context.Context, in sdkconfig.Input) (sdktool.Tool, error) {
	runner, err := sandboxRunner(in)
	if err != nil {
		return nil, err
	}
	return New(runner)
}

func sessionFactory(_ context.Context, in sdkconfig.Input) (sdktool.Tool, error) {
	runner, err := sandboxRunner(in)
	if err != nil {
		return nil, err
	}
	pm := sandbox.ProcessManagerOf(runner)
	if pm == nil {
		return nil, errdefs.NotAvailablef(
			"exec: sandbox runner does not implement ProcessManager; %s requires an interactive-capable backend",
			SessionName)
	}
	return NewSession(pm)
}

// sandboxRunner resolves the tool.Assembly resource's sandbox dep. It
// is deliberately strict: an exec tool without a runner would be a
// host-shell fallback, which this package never allows.
func sandboxRunner(in sdkconfig.Input) (sandbox.Runner, error) {
	value, ok := in.Dep(toolconfig.DepSandbox)
	if !ok {
		return nil, errdefs.Validationf(
			"exec builtin: tool.Assembly resource dep %q is not bound; declare deps: {sandbox: boxes/<name>}",
			toolconfig.DepSandbox)
	}
	runner, ok := value.(sandbox.Runner)
	if !ok {
		return nil, errdefs.Validationf(
			"exec builtin: tool.Assembly dep %q is %T, want sandbox.Runner",
			toolconfig.DepSandbox, value)
	}
	return runner, nil
}
