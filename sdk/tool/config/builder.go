package config

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/middleware"
)

// MiddlewareFactory instantiates one middleware from its own opaque
// spec. Decode it with [DecodeSpec]: unknown fields must be an error so
// typos in configuration surface at build time, not as silently ignored
// policy.
type MiddlewareFactory = sdkconfig.Func[sdkconfig.Input, tool.Middleware]

// Deps carries the application-side dependencies the document cannot
// express: who approves gated calls and where audit records go. A
// document using kind approval/audit without the corresponding Dep
// fails at Build, never at request time.
type Deps struct {
	Approver  middleware.Approver
	AuditSink middleware.AuditSink
}

// RegistryDep is the Input.Deps key under which Build exposes the
// freshly created Registry to middleware factories. Factories that need
// the catalog — timeout, rate limit, or a custom kind — read it here.
const RegistryDep = "registry"

// Builder turns validated Documents into Assemblies. It owns the
// builtin tool catalog, the middleware catalog (preloaded with the
// seven built-ins), and the source factory catalog — and Build creates
// the final Registry itself, so the document is the single description
// of the tool surface. No global registration: two Builders in one
// process stay independent.
type Builder struct {
	deps            Deps
	builtins        map[string]tool.Tool
	middlewares     *sdkconfig.Registry[sdkconfig.Input, tool.Middleware]
	sourceFactories map[string]SourceFactory
}

// NewBuilder returns a Builder with the built-in middleware factories
// and the builtin source kind registered. External source kinds are
// opt-in via RegisterSourceFactory, since each pulls in an external
// dependency.
func NewBuilder(deps Deps) *Builder {
	b := &Builder{
		deps:            deps,
		builtins:        make(map[string]tool.Tool),
		middlewares:     sdkconfig.NewRegistry[sdkconfig.Input, tool.Middleware](),
		sourceFactories: make(map[string]SourceFactory),
	}
	b.registerBuiltins()
	b.sourceFactories[BuiltinKind] = b.builtinSourceFactory
	return b
}

// RegisterBuiltin adds a hand-written Go tool to the builtin catalog.
// The document enables it by name through a builtin source entry;
// naming a tool that is not registered fails at Build. Nil tools, empty
// names, and duplicates are programming bugs.
func (b *Builder) RegisterBuiltin(t tool.Tool) {
	if isNilTool(t) {
		panic("config.RegisterBuiltin: tool is nil")
	}
	name := t.Definition().Name
	if name == "" {
		panic("config.RegisterBuiltin: tool name is empty")
	}
	if _, exists := b.builtins[name]; exists {
		panic(fmt.Sprintf("config.RegisterBuiltin: tool %q is already registered", name))
	}
	b.builtins[name] = t
}

// RegisterBuiltins registers several hand-written Go tools at once.
func (b *Builder) RegisterBuiltins(tools ...tool.Tool) {
	for _, t := range tools {
		b.RegisterBuiltin(t)
	}
}

// RegisterFactory adds a factory for kind. Empty kinds, nil factories,
// and duplicate registrations are programming bugs.
func (b *Builder) RegisterFactory(kind string, factory MiddlewareFactory) {
	if err := b.middlewares.Register(kind, factory); err != nil {
		panic(err)
	}
}

// Assembly is the result of building a document: the Executor to
// dispatch through, the Registry Build created, plus ownership of any
// attached sources.
//
// It exists because sources are live resources. A build that spawned MCP
// child processes has to hand back something closeable, or those
// processes outlive the configuration that created them with no handle
// to reach them by. Close is therefore mandatory whenever a document
// declares external sources, and harmless when it does not.
type Assembly struct {
	// Executor dispatches calls against the built Registry through the
	// document's middleware chain.
	Executor *tool.Executor

	// Catalog is the Registry Build created. Consumers that must
	// resolve tool names into definitions before calling — an LLM
	// request builder, for one — need the catalog, not the executor, so
	// the assembly exposes both halves.
	Catalog tool.Catalog

	sources []Source
}

// Close releases every attached source, unregistering their tools.
// Errors from individual sources are joined so one stuck child process
// does not hide the rest. The Executor remains usable afterwards but
// calls to a closed source's tools will fail.
func (a *Assembly) Close() error {
	if a == nil {
		return nil
	}
	var errs []error
	for _, v := range slices.Backward(a.sources) {
		if err := v.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	a.sources = nil
	return errors.Join(errs...)
}

// Build assembles the Executor and the Registry.
//
// A fresh Registry is created for every build, so the document fully
// determines the tool surface: builtin source entries resolve
// hand-written Go tools from the catalog, external sources attach
// before scopes are applied, and the middleware chain is instantiated
// last, in document order. Building the same document twice yields two
// independent registries.
//
// The returned Assembly owns the Registry and any attached sources and
// must be closed by the caller. A failure partway through closes
// whatever already attached, so an error never leaves child processes
// behind.
func (b *Builder) Build(ctx context.Context, doc Document) (*Assembly, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	registry := tool.NewRegistry()
	sources, err := b.buildSources(ctx, doc.Sources, registry)
	if err != nil {
		return nil, err
	}
	assembly := &Assembly{sources: sources}

	for name, scope := range doc.Scopes {
		registered, ok := registry.Get(name)
		if !ok {
			_ = assembly.Close()
			return nil, errdefs.Validation(fmt.Errorf(
				"tool config scopes[%q]: tool is not registered", name,
			))
		}
		registry.RegisterWithScope(registered, scope)
	}
	chain := make([]tool.Middleware, 0, len(doc.Middlewares))
	for i, entry := range doc.Middlewares {
		mw, err := b.middlewares.Build(ctx, entry.Kind, sdkconfig.Input{
			Settings: entry.Spec,
			Deps:     map[string]any{RegistryDep: registry},
		})
		if err != nil {
			_ = assembly.Close()
			if errdefs.IsNotFound(err) {
				return nil, errdefs.Validation(fmt.Errorf(
					"tool config middlewares[%d]: unknown kind %q",
					i, entry.Kind))
			}
			return nil, fmt.Errorf(
				"tool config middlewares[%d] (%s): %w", i, entry.Kind, err,
			)
		}
		chain = append(chain, mw)
	}
	assembly.Executor = tool.NewExecutor(registry, chain...)
	assembly.Catalog = registry
	return assembly, nil
}

func isNilTool(t tool.Tool) bool {
	if t == nil {
		return true
	}
	value := reflect.ValueOf(t)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
