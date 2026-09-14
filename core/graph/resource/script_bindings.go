package resource

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/errdefs"
	scriptnode "github.com/GizClaw/flowcraft/core/graph/nodes/script"
	res "github.com/GizClaw/flowcraft/core/resource"
)

// ScriptBindingsFactory builds the standard script surface as an
// "agent.ScriptBindings" resource: board, expr, host, run, tools,
// inference, node, stream, parallel, plus the opt-in fs/shell globals
// whose capabilities it is wired to, and the late runtime global.
//
// A graph engine consumes it under its optional "script_bindings" dep:
//
//	resources:
//	  std:
//	    kind: agent.ScriptBindings
//	    impl: standard
//	    deps: {tools: tools, workspace: ws}
//	agents:
//	  assistant:
//	    engine:
//	      impl: graph
//	      deps: {script_runtime: js, script_bindings: std}
//
// The provider is built once and shared by every script execution of
// every engine that references it, so per-execution state (board, node
// identity, executing runtime) arrives through
// [bindings.Invocation] rather than through the resource.
type ScriptBindingsFactory struct{}

// ScriptBindingsSettings is the strict settings subtree of the standard
// script surface: the policy of the bridges whose shapes the surface
// owns. Omitting a section keeps the bridge default — an allow-list is
// never implied, so a surface without settings stays fail-closed on
// tools and shell.
type ScriptBindingsSettings struct {
	// Tools configures the "tools" global; requires the tools dep.
	Tools *ScriptToolsSettings `json:"tools,omitempty"`

	// FS configures the "fs" global's size caps; requires the
	// workspace dep.
	FS *ScriptFSSettings `json:"fs,omitempty"`

	// Shell configures the "shell" global's command policy; requires
	// the sandbox dep.
	Shell *ScriptShellSettings `json:"shell,omitempty"`
}

// ScriptToolsSettings is the "tools" section of the standard script
// surface.
type ScriptToolsSettings struct {
	// Allow is the exact set of catalog tools the surface may call.
	// Every name must exist in the wired tool assembly, so a typo fails
	// the build instead of the first call. An explicit empty list
	// denies every tool.
	Allow []string `json:"allow,omitempty"`

	// AllowAll exposes every tool in the catalog. Use only when the
	// scripts in this deployment are fully trusted. It conflicts with
	// Allow.
	AllowAll bool `json:"allow_all,omitempty"`
}

// ScriptFSSettings is the "fs" section of the standard script surface.
type ScriptFSSettings struct {
	// MaxReadBytes caps one fs.read; positive. Omitting keeps the
	// bridge default.
	MaxReadBytes *int64 `json:"max_read_bytes,omitempty"`

	// MaxWriteBytes caps one fs.write; positive. Omitting keeps the
	// bridge default.
	MaxWriteBytes *int64 `json:"max_write_bytes,omitempty"`
}

// ScriptShellSettings is the "shell" section of the standard script
// surface.
type ScriptShellSettings struct {
	// Allow restricts shell.exec to these commands, matched against
	// the command as written and against its base name. An explicit
	// empty list denies every command; omitting the section leaves the
	// sandbox runner's own policy in charge.
	Allow []string `json:"allow,omitempty"`
}

// Spec implements res.Factory.
func (ScriptBindingsFactory) Spec() res.Spec {
	return res.Spec{
		Kind: bindings.ResourceKind,
		Impl: scriptnode.StandardImpl,
		Deps: []res.DepSpec{
			{Name: DepTools, Type: "tool.Assembly"},
			{Name: DepInference, Type: "inference.Assembly"},
			{Name: DepRouter, Type: "inference.Router"},
			{Name: DepWorkspace, Type: "workspace.Workspace"},
			{Name: DepSandbox, Type: "sandbox.Runner"},
		},
	}
}

// New implements res.Factory.
func (ScriptBindingsFactory) New(ctx context.Context, in res.Input) (any, error) {
	settings, err := res.DecodeTyped[ScriptBindingsSettings](ctx, in.Settings)
	if err != nil {
		return nil, err
	}
	deps, err := decodeDependencies(in.Deps)
	if err != nil {
		return nil, err
	}
	toolOpts, fsOpts, shellOpts, err := scriptBridgeOptions(settings, deps)
	if err != nil {
		return nil, err
	}
	scriptDeps := scriptNodeDeps(deps, nil)
	scriptDeps.ToolOptions = toolOpts
	scriptDeps.FSOptions = fsOpts
	scriptDeps.ShellOptions = shellOpts
	return scriptnode.StandardBindings(scriptDeps), nil
}

// NoScriptBindingsImpl is the impl name of the provider that binds
// nothing: scripts still run, but their scope carries no globals at
// all — the explicit way to disable the script surface.
const NoScriptBindingsImpl = "none"

// ScriptBindingsNoneFactory builds an empty script surface as the
// "agent.ScriptBindings/none" resource.
type ScriptBindingsNoneFactory struct{}

// Spec implements res.Factory.
func (ScriptBindingsNoneFactory) Spec() res.Spec {
	return res.Spec{Kind: bindings.ResourceKind, Impl: NoScriptBindingsImpl}
}

// New implements res.Factory.
func (ScriptBindingsNoneFactory) New(context.Context, res.Input) (any, error) {
	return bindings.ProviderFunc(func(bindings.Invocation) ([]bindings.Binding, error) {
		return nil, nil
	}), nil
}

// scriptBridgeOptions turns the surface's settings into bridge options,
// validating them against the capabilities the resource is wired to: a
// policy for a bridge that cannot exist is a configuration error, not a
// silently ignored setting.
func scriptBridgeOptions(
	settings ScriptBindingsSettings,
	deps dependencies,
) ([]bindings.ToolBridgeOption, []bindings.FSBridgeOption, []bindings.ShellOption, error) {
	var (
		toolOpts  []bindings.ToolBridgeOption
		fsOpts    []bindings.FSBridgeOption
		shellOpts []bindings.ShellOption
	)

	if settings.Tools != nil {
		if deps.tools == nil {
			return nil, nil, nil, errdefs.Validationf(
				"script bindings: settings.tools requires the %q dep", DepTools)
		}
		names, err := policyNames(settings.Tools.Allow, "settings.tools.allow")
		if err != nil {
			return nil, nil, nil, err
		}
		switch {
		case settings.Tools.AllowAll && len(names) > 0:
			return nil, nil, nil, errdefs.Validationf(
				"script bindings: settings.tools.allow_all conflicts with settings.tools.allow")
		case settings.Tools.AllowAll:
			toolOpts = append(toolOpts, bindings.WithToolAllowAll())
		case settings.Tools.Allow != nil:
			catalog := deps.tools.Catalog()
			for _, name := range names {
				if _, ok := catalog.Get(name); !ok {
					return nil, nil, nil, errdefs.Validationf(
						"script bindings: settings.tools.allow names unknown tool %q", name)
				}
			}
			toolOpts = append(toolOpts, bindings.WithAllowedToolNames(names...))
		}
	}

	if settings.FS != nil {
		if deps.workspace == nil {
			return nil, nil, nil, errdefs.Validationf(
				"script bindings: settings.fs requires the %q dep", DepWorkspace)
		}
		if cap := settings.FS.MaxReadBytes; cap != nil {
			if *cap <= 0 {
				return nil, nil, nil, errdefs.Validationf(
					"script bindings: settings.fs.max_read_bytes must be > 0")
			}
			fsOpts = append(fsOpts, bindings.WithMaxReadBytes(*cap))
		}
		if cap := settings.FS.MaxWriteBytes; cap != nil {
			if *cap <= 0 {
				return nil, nil, nil, errdefs.Validationf(
					"script bindings: settings.fs.max_write_bytes must be > 0")
			}
			fsOpts = append(fsOpts, bindings.WithMaxWriteBytes(*cap))
		}
	}

	if settings.Shell != nil {
		if deps.sandbox == nil {
			return nil, nil, nil, errdefs.Validationf(
				"script bindings: settings.shell requires the %q dep", DepSandbox)
		}
		cmds, err := policyNames(settings.Shell.Allow, "settings.shell.allow")
		if err != nil {
			return nil, nil, nil, err
		}
		if settings.Shell.Allow != nil {
			shellOpts = append(shellOpts, bindings.WithAllowedCommands(cmds...))
		}
	}

	return toolOpts, fsOpts, shellOpts, nil
}

// policyNames validates one policy list: entries must be non-empty.
func policyNames(list []string, field string) ([]string, error) {
	for i, name := range list {
		if name == "" {
			return nil, errdefs.Validationf(
				"script bindings: %s[%d] must be non-empty", field, i)
		}
	}
	return list, nil
}
