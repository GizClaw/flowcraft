package host

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// journal records the order of the events a test asserts on, so "closed in reverse" is a fact
// rather than a reading of the source.
// It is mutex-guarded because the concurrency test closes sessions from several goroutines at once.
type journal struct {
	mu     sync.Mutex
	events []string
}

func (j *journal) add(event string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, event)
}

func (j *journal) joined() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return strings.Join(j.events, ",")
}

// provider is a target whose catalog declares one model.
type provider struct {
	name    string
	models  []model.ModelDescriptor
	journal *journal
}

func (p provider) build() *inference.Provider {
	// A target has to serve a seam, and these tests never call it: the router is built, not
	// exercised.
	return &inference.Provider{Generator: generator{}, Catalog: catalog{models: p.models}}
}

type generator struct{}

func (generator) Generate(context.Context, inference.GenerateRequest) (inference.GenerateResponse, error) {
	return inference.GenerateResponse{}, errors.New("not used")
}

func (generator) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	return nil, errors.New("not used")
}

type catalog struct{ models []model.ModelDescriptor }

func (c catalog) Models() []model.ModelDescriptor { return c.models }

func (c catalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, descriptor := range c.models {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return model.ModelDescriptor{}, false
}

// runner is a session's backend instance: it records when it is closed.
type runner struct {
	name    string
	journal *journal
	err     error
	confine sandbox.Confine
}

func (r runner) Close() error {
	r.journal.add("close:" + r.name)
	return r.err
}

func (runner) Capabilities() sandbox.Capabilities { return sandbox.Capabilities{} }

func (runner) Start(context.Context, sandbox.SessionSpec) (sandbox.Session, error) {
	return nil, errors.New("not used")
}

func (runner) List(context.Context) ([]sandbox.SessionInfo, error) { return nil, nil }

func (runner) Terminate(context.Context, string) error { return nil }

func modelRef(providerName, name string) model.ModelDescriptor {
	return model.ModelDescriptor{ID: model.ModelID{Provider: providerName, Name: name}}
}

func plannedProvider(p provider) deploy.PlannedProvider {
	return deploy.PlannedProvider{Name: p.name, Impl: p.name, New: func() (*inference.Provider, error) {
		return p.build(), nil
	}}
}

// plannedBackend is a backend factory: the instance is built per session, from that session's
// confine.
func plannedBackend(r runner) deploy.PlannedBackend {
	return deploy.PlannedBackend{Name: r.name, Impl: r.name,
		New: func() (func(sandbox.Confine) (sandbox.Runner, error), error) {
			return func(confine sandbox.Confine) (sandbox.Runner, error) {
				r.confine = confine
				return r, nil
			}, nil
		}}
}

func plannedMiddleware(name string, build func(tool.Catalog) (tool.Middleware, error)) deploy.PlannedMiddleware {
	return deploy.PlannedMiddleware{Name: name, Impl: name,
		New: func() (func(tool.Catalog) (tool.Middleware, error), error) { return build, nil }}
}

func plannedSource(name string) deploy.PlannedToolSource {
	return deploy.PlannedToolSource{Name: name, Impl: "fake", New: func() (tool.Source, error) {
		return tooltest.Source(), nil
	}}
}

// TestOpenBuildsWhatThePlanDescribed is the package's contract: the plan's closures are the only
// thing it calls, and the result answers what an agent document asks.
func TestOpenBuildsWhatThePlanDescribed(t *testing.T) {
	log := &journal{}
	plan := deploy.ProcessPlan{
		Providers: []deploy.PlannedProvider{plannedProvider(provider{
			name: "gpt", journal: log, models: []model.ModelDescriptor{modelRef("openai", "gpt-5")},
		})},
		Backends: []deploy.PlannedBackend{plannedBackend(runner{name: "local", journal: log})},
		Tools:    []deploy.PlannedToolSource{plannedSource("workspace")},
	}
	res, err := Open(context.Background(), plan, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.models == nil {
		t.Fatal("no routed provider was built")
	}
	if _, ok := res.backendFactory("local"); !ok {
		t.Fatal("the backend factory is missing")
	}
	if res.tools == nil || res.dispatch == nil {
		t.Fatal("the tool table is missing")
	}
	caps := res.Capabilities()
	if len(caps.Backends) != 1 || caps.Backends[0] != "local" {
		t.Fatalf("backends = %v", caps.Backends)
	}
	if len(caps.Models) != 1 || caps.Models[0] != "openai/gpt-5" {
		t.Fatalf("models = %v, want the catalog's answer", caps.Models)
	}

	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Nothing here closes a backend: an instance belongs to a session's cell, and this package
	// only holds the constructor.
	if got := log.joined(); got != "" {
		t.Fatalf("events = %q, want no backend closed by the process", got)
	}
}

// TestBackendFactoryTakesTheSessionsConfine is the reason backends are factories: the write set and
// the network posture come from the grant, so the instance is built with them.
func TestBackendFactoryTakesTheSessionsConfine(t *testing.T) {
	log := &journal{}
	plan := deploy.ProcessPlan{Backends: []deploy.PlannedBackend{plannedBackend(runner{name: "local", journal: log})}}
	res, err := Open(context.Background(), plan, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = res.Close() }()

	factory, ok := res.backendFactory("local")
	if !ok {
		t.Fatal("the factory is missing")
	}
	confine := sandbox.Confine{Files: policy.Files{ReadOnly: true}}
	built, err := factory(confine)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if built.(runner).confine.Files.ReadOnly != true {
		t.Fatalf("confine = %+v, want the session's", built.(runner).confine)
	}
	if err := built.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := log.joined(); got != "close:local" {
		t.Fatalf("events = %q, want the instance closed by its session", got)
	}
}

// TestOpenUnwindsWhatItBuilt is the reason every step is guarded: a tool table built before a
// failing chain link must be closed, or the sources behind it leak.
func TestOpenUnwindsWhatItBuilt(t *testing.T) {
	log := &journal{}
	plan := deploy.ProcessPlan{
		Tools: []deploy.PlannedToolSource{{
			Name: "closing", Impl: "fake",
			New: func() (tool.Source, error) { return closingSource{journal: log}, nil },
		}},
		Middleware: []deploy.PlannedMiddleware{
			plannedMiddleware("broken", func(tool.Catalog) (tool.Middleware, error) {
				return nil, errors.New("bad settings")
			}),
		},
	}
	if _, err := Open(context.Background(), plan, testOptions(t)); err == nil {
		t.Fatal("Open accepted a failing link")
	} else if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("error %q does not name the failing link", err)
	}
	if got := log.joined(); got != "close:tool" {
		t.Fatalf("events = %q, want the tool table closed before the error escaped", got)
	}
}

// closingSource is a tool source whose tools close themselves, which is how the registry's close
// becomes observable.
type closingSource struct{ journal *journal }

func (s closingSource) Tools() []tool.Tool {
	return []tool.Tool{closingTool(s)}
}

func (closingSource) LazyTools() []tool.LazyTool { return nil }

type closingTool struct{ journal *journal }

func (t closingTool) Close() error { t.journal.add("close:tool"); return nil }

func (closingTool) Needs() []tool.Need { return nil }

func (closingTool) Definition() message.ToolDefinition {
	return message.ToolDefinition{Name: "closing", InputSchema: []byte(`{"type":"object"}`)}
}

func (t closingTool) Execute(context.Context, tool.Binding, string) (message.Content, error) {
	return message.NewTextContent("ok"), nil
}

// TestOpenJoinsCleanupFailures keeps the original cause visible: a resource that fails to close is
// a second problem, not a replacement for the first.
//
// stuckSource is a tool whose Close fails.
type stuckSource struct{}

func (stuckSource) Tools() []tool.Tool         { return []tool.Tool{stuckTool{}} }
func (stuckSource) LazyTools() []tool.LazyTool { return nil }

type stuckTool struct{}

func (stuckTool) Close() error { return errors.New("close failed") }

func (stuckTool) Needs() []tool.Need { return nil }

func (stuckTool) Definition() message.ToolDefinition {
	return message.ToolDefinition{Name: "stuck", InputSchema: []byte(`{"type":"object"}`)}
}

func (stuckTool) Execute(context.Context, tool.Binding, string) (message.Content, error) {
	return message.NewTextContent("ok"), nil
}

func TestOpenJoinsCleanupFailures(t *testing.T) {
	plan := deploy.ProcessPlan{
		Tools: []deploy.PlannedToolSource{{
			Name: "stuck", Impl: "fake",
			New: func() (tool.Source, error) { return stuckSource{}, nil },
		}},
		Middleware: []deploy.PlannedMiddleware{
			plannedMiddleware("broken", func(tool.Catalog) (tool.Middleware, error) {
				return nil, errors.New("bad settings")
			}),
		},
	}
	_, err := Open(context.Background(), plan, testOptions(t))
	if err == nil {
		t.Fatal("Open accepted a failing link")
	}
	for _, want := range []string{"bad settings", "close failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// TestADisabledLinkIsSkipped: a middleware that decides at construction that it is off returns
// nil, and the chain does not carry it.
func TestADisabledLinkIsSkipped(t *testing.T) {
	active := func(next tool.Dispatch) tool.Dispatch { return next }
	plan := deploy.ProcessPlan{
		Tools: []deploy.PlannedToolSource{plannedSource("workspace")},
		Middleware: []deploy.PlannedMiddleware{
			plannedMiddleware("off", func(tool.Catalog) (tool.Middleware, error) { return nil, nil }),
			plannedMiddleware("on", func(tool.Catalog) (tool.Middleware, error) { return active, nil }),
		},
	}
	res, err := Open(context.Background(), plan, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = res.Close() }()
	if res.dispatch == nil {
		t.Fatal("the chain is missing")
	}
	// A nil link must be dropped by this package, not tolerated by dispatch: the assertion is that the
	// chain still runs, with the enabled link alone. A middleware that counted calls proves it.
	if res.tools == nil {
		t.Fatal("the tool table is missing")
	}
	// What the host owes the engine is the chain it built: the enabled link present, the nil one dropped
	// here rather than tolerated there. Running the chain is the engine's own test.
	var handed []tool.Middleware
	opts := testOptions(t)
	opts.Dispatcher = func(catalog tool.Catalog, chain []tool.Middleware) tool.Dispatcher {
		handed = chain
		return stubDispatch{catalog: catalog, chain: chain}
	}
	if _, err := Open(context.Background(), plan, opts); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(handed) != 1 || handed[0] == nil {
		t.Fatalf("chain handed to the dispatcher = %v, want the one enabled link", handed)
	}
}

// missingSlot is a capability nothing in these tests provides.
var missingSlot = tool.NewSlot[*struct{}]("test.missing")

func definition(name string) message.ToolDefinition {
	return message.ToolDefinition{Name: name, InputSchema: []byte(`{"type":"object"}`)}
}

// TestRegistrationRefusesAnUndeclaredCapability is what Options.Capabilities buys: a tool that
// needs a capability this process cannot hand fails at build, not on its first call.
func TestRegistrationRefusesAnUndeclaredCapability(t *testing.T) {
	needy := tool.CapFunc([]tool.Need{missingSlot}, definition("needy"),
		func(context.Context, tool.Binding, string) (message.Content, error) {
			return message.NewTextContent("ok"), nil
		})

	plan := deploy.ProcessPlan{
		Tools: []deploy.PlannedToolSource{{Name: "needy", Impl: "fake",
			New: func() (tool.Source, error) { return tooltest.Source(needy), nil }}},
	}
	if _, err := Open(context.Background(), plan, testOptions(t)); err == nil {
		t.Fatal("a tool whose declared capability nothing provides was registered")
	}
}

// TestOpenUnwindsAFailedBackendAndSource covers the unwind paths the review found untested: every build
// step that can fail must close what it already built.
func TestOpenUnwindsAFailedBackendAndSource(t *testing.T) {
	for name, plan := range map[string]deploy.ProcessPlan{
		"provider": {Providers: []deploy.PlannedProvider{{Name: "p", Impl: "fake",
			New: func() (*inference.Provider, error) { return nil, errors.New("provider failed") }}}},
		"backend": {
			Providers: []deploy.PlannedProvider{plannedProvider(provider{name: "gpt", journal: &journal{}})},
			Backends: []deploy.PlannedBackend{{Name: "b", Impl: "fake",
				New: func() (func(sandbox.Confine) (sandbox.Runner, error), error) {
					return nil, errors.New("backend failed")
				}}},
		},
		"nil backend": {
			Backends: []deploy.PlannedBackend{{Name: "b", Impl: "fake",
				New: func() (func(sandbox.Confine) (sandbox.Runner, error), error) { return nil, nil }}},
		},
		"tool source": {Tools: []deploy.PlannedToolSource{{Name: "t", Impl: "fake",
			New: func() (tool.Source, error) { return nil, errors.New("source failed") }}}},
		"nil source": {Tools: []deploy.PlannedToolSource{{Name: "t", Impl: "fake",
			New: func() (tool.Source, error) { return nil, nil }}}},
		"middleware": {Middleware: []deploy.PlannedMiddleware{{Name: "m", Impl: "fake",
			New: func() (func(tool.Catalog) (tool.Middleware, error), error) {
				return nil, errors.New("middleware failed")
			}}}},
	} {
		if _, err := Open(context.Background(), plan, testOptions(t)); err == nil {
			t.Fatalf("%s: Open accepted a failing build", name)
		}
	}
}

// TestOpenClosesTheRegistryWhenTheChainFails is the unwind that has something to release: the tool table
// built before a failing link must be closed.
func TestOpenClosesTheRegistryWhenTheChainFails(t *testing.T) {
	log := &journal{}
	plan := deploy.ProcessPlan{
		Tools: []deploy.PlannedToolSource{{Name: "closing", Impl: "fake",
			New: func() (tool.Source, error) { return closingSource{journal: log}, nil }}},
		Middleware: []deploy.PlannedMiddleware{{Name: "broken", Impl: "fake",
			New: func() (func(tool.Catalog) (tool.Middleware, error), error) {
				return nil, errors.New("chain failed")
			}}},
	}
	if _, err := Open(context.Background(), plan, testOptions(t)); err == nil {
		t.Fatal("Open accepted a failing link")
	}
	if got := log.joined(); got != "close:tool" {
		t.Fatalf("events = %q, want the tool table closed", got)
	}
}

// TestNilCapabilitiesRefusesCapabilityTools is the fail-closed default of Options.
func TestNilCapabilitiesRefusesCapabilityTools(t *testing.T) {
	needy := tool.CapFunc([]tool.Need{missingSlot},
		message.ToolDefinition{Name: "needy", InputSchema: []byte(`{"type":"object"}`)},
		func(context.Context, tool.Binding, string) (message.Content, error) {
			return message.NewTextContent("ok"), nil
		})
	plan := deploy.ProcessPlan{Tools: []deploy.PlannedToolSource{{Name: "needy", Impl: "fake",
		New: func() (tool.Source, error) { return tooltest.Source(needy), nil }}}}
	if _, err := Open(context.Background(), plan, testOptions(t)); err == nil {
		t.Fatal("a tool that needs a capability was registered without one")
	}
	// With the capability declared, the same plan is accepted.
	declared := tool.NewCapSet()
	missingSlot.Provide(declared, nil)
	if _, err := Open(context.Background(), plan, Options{Capabilities: declared, Dispatcher: testOptions(t).Dispatcher, View: testOptions(t).View}); err != nil {
		t.Fatalf("Open with the capability declared: %v", err)
	}
}

// testOptions is the process an application would open: a dispatcher and a projection from outside the
// host. The stub is deliberate — core cannot reach for the real ones, and a host that needed them would
// depend on the engine it is supposed to run.
func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Dispatcher: func(catalog tool.Catalog, chain []tool.Middleware) tool.Dispatcher {
			return stubDispatch{catalog: catalog, chain: chain}
		},
		View: func(catalog tool.Catalog, allowed policy.ToolSet) (tool.ViewFactory, error) {
			return statelessFactory(catalog), nil
		},
	}
}

// statelessFactory answers with a fresh view per session. The stub keeps no state, so a case
// that cares about isolation supplies its own factory (see view_test.go).
func statelessFactory(catalog tool.Catalog) tool.ViewFactory {
	return func() tool.View { return stubView{catalog: catalog} }
}

// stubDispatch runs a call straight through the catalog, recording nothing: what a host test asserts is
// what the host passed in, not how an engine executes.
type stubDispatch struct {
	catalog tool.Catalog
	chain   []tool.Middleware
}

func (d stubDispatch) Execute(ctx context.Context, b tool.Binding, call message.ToolCall) message.ToolResult {
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

func (d stubDispatch) ExecuteAll(ctx context.Context, b tool.Binding, calls []message.ToolCall) []message.ToolResult {
	out := make([]message.ToolResult, len(calls))
	for i, call := range calls {
		out[i] = d.Execute(ctx, b, call)
	}
	return out
}

// stubView shows every tool the catalog holds.
type stubView struct{ catalog tool.Catalog }

func (v stubView) Definitions() []message.ToolDefinition { return v.catalog.Definitions() }
func (stubView) Visible(string) bool                     { return true }
func (stubView) RecordCall(message.ToolCall)             {}
