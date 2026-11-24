// Package graphtest is the substrate a node kind or a script is tested against without a process.
//
// It exists because the point of the runtime seam is that a kind depends on four methods and not on
// the assembly that satisfies them: a test that had to build a sandbox, an inference provider and a
// tool source to call one node would be testing the assembly, and it would break every time the
// assembly changed. Here the tools are real — they go through the same registry, projection and
// dispatcher a deployed process uses — and everything around them is a value the case supplies.
package graphtest

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"

	"github.com/GizClaw/flowcraft/core/tool/projection"
)

// Fixture is one session's substrate for a case: a projection over the test's tools, the dispatcher
// that executes them, and the model access the case supplied.
//
// It is not an interface a kind reads. Node kinds receive their substrate with each invocation, so the
// fixture's job is to hand those four values to an [graph.Invoke] — the same thing a run does for a
// deployed node.
type Fixture struct {
	// Root is the directory the case may write in. A tool that takes a path uses it, which keeps a
	// case from touching the machine it runs on.
	Root string
	// Projection is the process-wide projection a new session view comes from.
	Projection *projection.Projection
	// View is the session's projection. A case that wants to simulate a discovery calls
	// View.Discover before invoking the node.
	View *projection.View

	binding tool.Binding
	tools   tool.Dispatcher
	models  *inference.Provider
	def     model.ModelRef
}

// Substrate returns the four values a run attaches to every invocation, for a case that drives a
// request through a real program and wants the same authority the fixture's stamps carry.
func (f *Fixture) Substrate() (tool.Binding, tool.Dispatcher, *inference.Provider, model.ModelRef) {
	return f.binding, f.tools, f.models, f.def
}

// Stamp attaches this fixture's substrate to an invocation, which is what a run does for every node
// it invokes.
func (f *Fixture) Stamp(in *graph.Invoke) {
	in.Binding = f.binding
	in.Tools = f.tools
	in.Models = f.models
	in.DefaultModel = f.def
}

// Config is one test runtime's inputs.
type Config struct {
	// Generator is the model access. Nil means the case makes no model call.
	Generator inference.Generator
	// Tools are the tools the session may call, registered in order.
	Tools []tool.Tool
	// Lazy are tools that arrive through discovery.
	Lazy []tool.LazyTool
	// Deferred hides every tool behind tool_search, so a case can prove that a discovery is what
	// made something visible.
	Deferred bool
	// Model is what a node calls when its document names none.
	Model model.ModelRef
}

// New builds a runtime over cfg.
func New(t testing.TB, cfg Config) *Fixture {
	t.Helper()
	registry, err := tool.NewRegistry([]tool.Source{source{tools: cfg.Tools, lazy: cfg.Lazy}})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	exposure := projection.Policy{Default: projection.ExposureAlways}
	if cfg.Deferred {
		exposure = projection.DefaultPolicy()
	}
	projected, err := projection.New(registry, exposure)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	view := projected.NewView()
	return &Fixture{
		Root:       t.TempDir(),
		Projection: projected,
		View:       view,
		binding:    tool.Bind(session{}, policy.Grant{}).WithView(view),
		tools:      known{registry},
		models:     &inference.Provider{Generator: cfg.Generator},
		def:        cfg.Model,
	}
}

// session is the tool.Session a case's binding carries: an address and nothing else.
type session struct{}

func (session) ID() string            { return "case" }
func (session) Scratch() tool.Scratch { return scratch{} }

type scratch struct{}

func (scratch) Load(string) (any, bool) { return nil, false }
func (scratch) Store(string, any)       {}
func (scratch) Delete(string)           {}

// source adapts tools and lazy descriptors into a tool.Source.
type source struct {
	tools []tool.Tool
	lazy  []tool.LazyTool
}

func (s source) Tools() []tool.Tool         { return s.tools }
func (s source) LazyTools() []tool.LazyTool { return s.lazy }

// Echo returns a tool that answers with its arguments, which is what a case needs to prove that a
// call's payload survived the trip.
func Echo() tool.Tool {
	return tool.TextTool(message.ToolDefinition{
		Name:        "echo",
		Description: "echo the request",
	}, func(_ context.Context, arguments string) (string, error) {
		return arguments, nil
	})
}

// Failing returns a tool that always reports message, for the failure path of a call.
func Failing(name, text string) tool.Tool {
	return tool.TextTool(message.ToolDefinition{Name: name, Description: "always fails"},
		func(context.Context, string) (string, error) {
			return "", complaint(text)
		})
}

type complaint string

func (c complaint) Error() string { return string(c) }

// known is the smallest dispatcher a fixture needs: resolve the tool and call it. A node test asserts
// what a kind does with a call, not how an engine routes one, so the engine stays out of the fixture.
type known struct{ catalog tool.Catalog }

func (d known) Execute(ctx context.Context, b tool.Binding, call message.ToolCall) message.ToolResult {
	target, ok := d.catalog.Get(call.Name)
	if !ok {
		return message.NewErrorToolResult(call.ID, "no such tool")
	}
	content, err := target.Execute(ctx, b, call.Arguments.String())
	if err != nil {
		return message.NewErrorToolResult(call.ID, err.Error())
	}
	return message.NewToolResult(call.ID, content)
}

func (d known) ExecuteAll(ctx context.Context, b tool.Binding, calls []message.ToolCall) []message.ToolResult {
	out := make([]message.ToolResult, len(calls))
	for i, call := range calls {
		out[i] = d.Execute(ctx, b, call)
	}
	return out
}
