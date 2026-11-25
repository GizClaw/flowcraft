package deploy

import (
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// Group is which table a factory belongs to. A document entry names the implementation, and
// that name only means something within its group: an implementation called "local" is a
// different thing as a provider and as a backend.
type Group int

const (
	// GroupProvider is a model provider target.
	GroupProvider Group = iota
	// GroupBackend is a sandbox backend.
	GroupBackend
	// GroupToolSource is a contributor of tools.
	GroupToolSource
	// GroupMiddleware is one link of the process's tool chain.
	GroupMiddleware
	// GroupHook is one of the host policies attached to an agent's lifecycle.
	GroupHook
	// GroupProgram is what runs a conversation.
	GroupProgram
)

// String renders the group as a document section spells it.
func (g Group) String() string {
	switch g {
	case GroupProvider:
		return "provider"
	case GroupBackend:
		return "backend"
	case GroupToolSource:
		return "tool_source"
	case GroupMiddleware:
		return "middleware"
	case GroupHook:
		return "hook"
	case GroupProgram:
		return "program"
	default:
		return fmt.Sprintf("Group(%d)", int(g))
	}
}

// Factory is one registered implementation, split into the two phases a deployment needs.
//
// [Factory.Plan] runs while the document is loaded: it decodes the entry's settings
// strictly, so a typo inside a settings block is a document error and is reported with the
// rest of them. [Factory.Make] runs when the process is built. Keeping them apart is what
// lets a document be validated in a test, or a CI job, without dialling anything.
type Factory struct {
	// Name is the implementation as the document writes it.
	Name string
	// Plan decodes and validates the settings. It must not have side effects.
	Plan func(raw json.RawMessage) (any, error)
	// Make builds the object from what Plan returned.
	Make func(settings any) (any, error)
	// Trace is the least trace level this implementation is worth installing at. Zero means
	// always.
	//
	// It is the registration's own statement of cost, not the loader's guess: only the
	// application knows that its telemetry link produces the very spans a lower level promised
	// to skip. The loader drops the link instead of building one that turns itself off, which
	// is what keeps the level a construction decision rather than a per-call branch.
	Trace telemetry.TraceLevel
	// Enforcement is what a backend implementation can enforce on the platform this binary was
	// built for. It is meaningless for every other group.
	//
	// It is declared here because the loader has to answer "does this deployment's backend
	// bound writes" while reading the document, and a per-session instance — the only other
	// place the answer exists — is built later, by the host, for one confine. A backend that
	// declares nothing is a backend nothing can be escalated on: the conservative answer is
	// the one a missing declaration gets.
	Enforcement sandbox.Enforcement
}

// AtTrace declares the least trace level this implementation is worth installing at.
//
//	impls.Middleware(deploy.MiddlewareImpl("telemetry", build).AtTrace(telemetry.TraceCall))
func (f Factory) AtTrace(level telemetry.TraceLevel) Factory {
	f.Trace = level
	return f
}

// Enforcing declares what a backend implementation can enforce on this platform.
//
//	impls.Backend(deploy.BackendImpl("bwrap", bwrap.New).
//		Enforcing(bwrap.Capabilities().Policy))
//
// It travels into the process's capabilities, which is what lets an agent document that
// declares an escalation be refused at load on a backend with no boundary to exceed.
func (f Factory) Enforcing(enforcement sandbox.Enforcement) Factory {
	f.Enforcement = enforcement
	return f
}

// Impl builds a factory for one implementation, typed at both ends.
//
//	// the application, not core
//	impls.Provider(deploy.Impl("openai", openai.New))
//
// The `any` never escapes this package: Plan returns the settings type the document
// described, and Make asserts it back before calling build, so a registered factory cannot
// be handed settings of another implementation.
func Impl[S any, T any](name string, build func(S) (T, error)) Factory {
	if name == "" {
		panic("deploy.Impl: a factory needs a name")
	}
	if build == nil {
		panic("deploy.Impl: " + name + " needs a constructor")
	}
	return Factory{
		Name: name,
		Plan: func(raw json.RawMessage) (any, error) {
			var settings S
			if len(raw) > 0 {
				if err := decodeSettings(raw, &settings); err != nil {
					return nil, err
				}
			}
			return settings, nil
		},
		Make: func(settings any) (any, error) {
			typed, ok := settings.(S)
			if !ok {
				return nil, errdefs.Internalf(
					"deploy: impl %q was planned with settings of another type", name)
			}
			built, err := build(typed)
			if err != nil {
				return nil, err
			}
			return built, nil
		},
	}
}

// backendFactory builds one session's backend instance.
//
// It is a closure rather than the runner itself because a backend serves exactly one session:
// the confine — what the grant allows for files, network, environment and resources — is fixed
// when that boundary is built, which is per session, while the factory is process-wide.
type backendFactory = func(sandbox.Confine) (sandbox.Runner, error)

// BackendImpl builds a factory for a sandbox backend.
func BackendImpl[S any](name string, build func(S, sandbox.Confine) (sandbox.Runner, error)) Factory {
	return Impl(name, func(settings S) (backendFactory, error) {
		return func(confine sandbox.Confine) (sandbox.Runner, error) {
			return build(settings, confine)
		}, nil
	})
}

// middlewareFactory builds one link of the tool chain.
type middlewareFactory = func(tool.Catalog) (tool.Middleware, error)

// MiddlewareImpl builds a factory for one link of the tool chain.
//
// The constructor receives the tool catalog because per-tool policy has nowhere else to read a
// tool's metadata: at call time a middleware sees the context, the binding and the request, and
// nothing about the tool it is about to run.
func MiddlewareImpl[S any](name string, build func(S, tool.Catalog) (tool.Middleware, error)) Factory {
	return Impl(name, func(settings S) (middlewareFactory, error) {
		return func(catalog tool.Catalog) (tool.Middleware, error) {
			return build(settings, catalog)
		}, nil
	})
}

// Impls is the application's registration table: which implementations this binary carries.
//
// It is assembled in code and holds no state beyond the table. Registration errors
// accumulate instead of being returned one at a time, because a deployment with three
// misregistered kinds should hear about all three.
type Impls struct {
	groups map[Group]map[string]Factory
	errs   []error
}

// NewImpls returns an empty table.
func NewImpls() *Impls { return &Impls{groups: map[Group]map[string]Factory{}} }

// Provider registers a model provider target.
func (k *Impls) Provider(f Factory) *Impls { return k.add(GroupProvider, f) }

// Backend registers a sandbox backend.
func (k *Impls) Backend(f Factory) *Impls { return k.add(GroupBackend, f) }

// ToolSource registers a contributor of tools.
func (k *Impls) ToolSource(f Factory) *Impls { return k.add(GroupToolSource, f) }

// Middleware registers one link of the tool chain. It is a registration because the chain is
// process-wide: one runtime serves every agent, so the links it applies to every tool call are
// the process's decision, not a deployment's.
func (k *Impls) Middleware(f Factory) *Impls { return k.add(GroupMiddleware, f) }

// Hook registers one lifecycle policy a deployment may attach by name. Hooks are behaviour,
// so the implementation is the application's; the document writes the name and the settings.
func (k *Impls) Hook(f Factory) *Impls { return k.add(GroupHook, f) }

// Program registers what runs a conversation.
func (k *Impls) Program(f Factory) *Impls { return k.add(GroupProgram, f) }

// Err joins every registration error. A loader refuses to run with a broken table rather
// than discovering it per entry.
func (k *Impls) Err() error { return errors.Join(k.errs...) }

// Names lists the kinds registered in one group, sorted, for a capability description or an
// error message.
func (k *Impls) Names(group Group) []string {
	registered := k.groups[group]
	names := make([]string, 0, len(registered))
	for name := range registered {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// factory returns the registered factory for an implementation.
func (k *Impls) factory(group Group, impl string) (Factory, bool) {
	if k == nil {
		return Factory{}, false
	}
	registered := k.groups[group]
	f, ok := registered[impl]
	return f, ok
}

func (k *Impls) add(group Group, f Factory) *Impls {
	if k.groups[group] == nil {
		k.groups[group] = map[string]Factory{}
	}
	if _, exists := k.groups[group][f.Name]; exists {
		k.errs = append(k.errs, errdefs.Validationf(
			"deploy: %s impl %q is registered twice; registration must be unambiguous",
			group, f.Name))
		return k
	}
	k.groups[group][f.Name] = f
	return k
}
