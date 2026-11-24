package app

import (
	"context"
	"fmt"
	"io"
	goruntime "runtime"
	"time"

	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/agent/graph/nodes"
	"github.com/GizClaw/flowcraft/agent/graph/script"
	"github.com/GizClaw/flowcraft/agent/graph/script/starlark"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/inference/anthropic"
	"github.com/GizClaw/flowcraft/inference/openai"
	"github.com/GizClaw/flowcraft/sandbox/local"
	"github.com/GizClaw/flowcraft/tool/builtin/exec"
	"github.com/GizClaw/flowcraft/tool/builtin/files"
	"github.com/GizClaw/flowcraft/tool/middleware"
)

// graphSettings is the graph engine's written form: the document it runs, and where a rule script's
// relative path resolves against.
type graphSettings struct {
	Path string `json:"path"`
	// Model is what a node calls when the graph names none. It is written here because the engine's
	// default is the engine's setting: the grant says which models a session may reach, and this says
	// which one a silence means.
	Model struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
	} `json:"model"`
	// Scripts is the directory a script node's `path` resolves against. It is the workspace, named by
	// the document, so a scenario writes `rules/vote_tally.star` and means the file beside it.
	Scripts string `json:"scripts,omitempty"`
}

// graphProgram is the engine a deployment selects with `program: {impl: graph}`.
//
// The graph is compiled here, once per deployment, which is what the two-level contract asks for: a
// session only opens a driver over an artefact that already exists.
func graphProgram(settings graphSettings) (agent.Program, error) {
	if settings.Path == "" {
		return nil, errdefs.Validationf("graph: settings.path is required")
	}
	source := &script.FileSource{BaseDir: settings.Scripts, TTL: time.Second}
	kind, err := script.New(
		script.WithBackends(starlark.New()),
		script.WithSource(source),
		script.WithLog(io.Discard),
	)
	if err != nil {
		return nil, fmt.Errorf("graph: script kind: %w", err)
	}
	// The decoders are the implementations this binary compiles in, so a node may write a provider's own
	// request option (`store`, `reasoning_effort`, a vendor's safety level) and have it checked against
	// what the driver actually serves. A document that names an option this build does not know is a
	// load error, not a silently ignored field.
	decoders := map[string]inference.ExtensionDecoder{}
	for key, decoder := range openai.Decoders() {
		decoders[key] = decoder
	}
	for key, decoder := range anthropic.Decoders() {
		decoders[key] = decoder
	}
	compiled, err := nodes.Open(context.Background(), settings.Path,
		config.File(settings.Path), nodes.WithKinds(kind), nodes.WithModelDecoders(decoders))
	if err != nil {
		return nil, err
	}
	option := graph.WithProgramDefaultModel(model.ModelRef{
		ID: model.ModelID{Provider: settings.Model.Provider, Name: settings.Model.Name},
	})
	return graph.NewProgram(compiled, option), nil
}

// filesSettings is the file tool's written form: whether the model may be handed an image.
type filesSettings struct {
	Images bool `json:"images"`
}

func builtinFiles(settings filesSettings) (tool.Source, error) {
	return files.New(files.WithImages(settings.Images)), nil
}

// execSettings is the command tool's written form.
type execSettings struct {
	// Shell overrides the platform shell; its name becomes the tool name.
	Shell string `json:"shell"`
}

func builtinExec(settings execSettings) (tool.Source, error) {
	command := exec.NewCommand()
	if settings.Shell != "" {
		command = exec.NewCommand(exec.WithShell(platformShell(settings.Shell)))
	}
	return oneTool{command}, nil
}

// oneTool adapts a single tool into a source: the exec command is one tool, and a deployment registers
// sources.
type oneTool struct{ tool.Tool }

func (s oneTool) Tools() []tool.Tool       { return []tool.Tool{s.Tool} }
func (oneTool) LazyTools() []tool.LazyTool { return nil }

// platformShell names a shell by its path, with the flags a one-shot command needs. The document names
// the path; how to speak to it is the platform's.
func platformShell(path string) sandbox.ShellSpec {
	fallback := sandbox.DefaultShell(goruntime.GOOS)
	spec, err := sandbox.ShellFor(fallback.Name, path, fallback.Args)
	if err != nil {
		return fallback
	}
	return spec
}

// middleware registers the chain links this binary offers. A deployment document lists the ones it
// wants, in order; a name that is not here fails the load.
func chainLinks(registered *deploy.Impls) *deploy.Impls {
	return registered.
		Middleware(deploy.MiddlewareImpl("recover", func(struct{}, tool.Catalog) (tool.Middleware, error) {
			return middleware.Recover(), nil
		})).
		Middleware(deploy.MiddlewareImpl("telemetry", func(struct{}, tool.Catalog) (tool.Middleware, error) {
			return middleware.Telemetry(), nil
		})).
		Middleware(deploy.MiddlewareImpl("truncate", func(settings truncateSettings, _ tool.Catalog) (tool.Middleware, error) {
			return middleware.Truncate(settings.TruncateSettings), nil
		}))
}

// truncateSettings is the truncation link's written form.
type truncateSettings struct {
	middleware.TruncateSettings
}

// sandboxBackends registers the backends this binary carries. The no-isolation runner is the only one
// that needs nothing from the host, and it is what a local demo uses; a hardened deployment registers
// its own.
func sandboxBackends(registered *deploy.Impls) *deploy.Impls {
	return registered.Backend(deploy.BackendImpl("local", func(_ struct{}, confine sandbox.Confine) (sandbox.Runner, error) {
		// The confine is the deployment's, not this function's: a backend is built for one session's
		// authority, and the registerer is the only place that authority exists.
		return local.New(confine)
	}))
}
