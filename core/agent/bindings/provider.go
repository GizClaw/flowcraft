package bindings

import (
	"context"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/utils/ptr"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// ResourceKind is the deployment resource kind of a script bindings
// provider: a resource of this kind produces a [Provider], and the
// graph engine consumes one under its "script_bindings" dep. The
// built-in "standard" impl is registered by core/graph/resource; hosts
// register their own impls to extend the script surface.
const ResourceKind = "agent.ScriptBindings"

// Binding is one named global produced for a script execution.
type Binding struct {
	// Name is the global's name in the script scope.
	Name string
	// Value is the global's value, typically a map[string]any of
	// script-callable functions (see [BindingFunc]).
	Value any
}

// Invocation carries the per-execution state a [Provider] needs to
// produce bindings.
//
// A provider is built once per deployment and shared across runs, so
// it cannot hold per-execution state: the current board, the node
// identity, the stream emitter and the executing runtime only exist
// while one node invocation is in flight. Bind receives them here.
type Invocation struct {
	// Context is the script execution context: the run's deadline and
	// cancellation, the ambient run identity, and — during a parallel
	// wave — the branch cancellation controller.
	Context context.Context

	// Board is the run's board.
	Board *agent.Board

	// Host is the run's host.
	Host agent.Host

	// Name is the execution label for this script: the node config's
	// "name", or the node id when unset.
	Name string

	// NodeID, NodeType and GraphID identify the invocation.
	NodeID   string
	NodeType string
	GraphID  string

	// RunInfo is the ambient run identity ([agent.RunInfoFromContext]).
	RunInfo agent.RunInfo

	// Runtime is the script runtime executing this invocation. The
	// standard "runtime" global wraps it for nested sub-scripts.
	Runtime agent.ScriptRuntime

	// Emit publishes a per-node stream delta. It is nil when the
	// invocation cannot emit; providers must treat that as "no
	// stream", not as an error.
	Emit func(agent.StreamDeltaPayload) error
}

// Provider supplies the script environment's globals for one
// execution. Implementations must be safe for concurrent use: one
// provider serves every script execution of every run of the engine
// that references it.
type Provider interface {
	// Bind returns the ordinary bindings for inv. A name bound twice —
	// inside one call or across the ordinary and late phases — fails
	// [Assemble] rather than silently shadowing.
	Bind(inv Invocation) ([]Binding, error)
}

// LateProvider is an optional [Provider] extension for bindings that
// must observe the environment built from the ordinary phase — the
// "runtime" global is the built-in case, because nested sub-scripts
// inherit the final bindings map.
type LateProvider interface {
	// BindLate returns bindings evaluated after all ordinary bindings
	// and receives the environment being built.
	BindLate(inv Invocation, env *agent.ScriptEnv) ([]Binding, error)
}

// ProviderFunc adapts a function to [Provider].
type ProviderFunc func(inv Invocation) ([]Binding, error)

// Bind implements [Provider].
func (f ProviderFunc) Bind(inv Invocation) ([]Binding, error) { return f(inv) }

// Chain composes providers into one: ordinary bindings run provider by
// provider in order, then each provider's late phase in the same
// order. Nil providers are skipped.
//
// Chain does not deduplicate names — [Assemble] rejects a name bound
// twice, so a collision between chained providers fails the execution
// instead of letting the later provider shadow the earlier one.
func Chain(providers ...Provider) Provider {
	filtered := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if p != nil {
			filtered = append(filtered, p)
		}
	}
	return chained(filtered)
}

type chained []Provider

// Bind implements [Provider].
func (c chained) Bind(inv Invocation) ([]Binding, error) {
	var out []Binding
	for _, p := range c {
		list, err := p.Bind(inv)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}

// BindLate implements [LateProvider].
func (c chained) BindLate(inv Invocation, env *agent.ScriptEnv) ([]Binding, error) {
	var out []Binding
	for _, p := range c {
		late, ok := p.(LateProvider)
		if !ok {
			continue
		}
		list, err := late.BindLate(inv, env)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}

// Assemble builds one execution's environment from provider: ordinary
// bindings first, then [LateProvider] bindings, with duplicate names
// rejected so a provider can never silently shadow another binding.
//
// A nil provider yields an empty environment carrying config — the
// script sees no globals at all.
func Assemble(inv Invocation, config map[string]any, provider Provider) (*agent.ScriptEnv, error) {
	env := &agent.ScriptEnv{
		Config:   config,
		Bindings: map[string]any{},
	}
	if provider == nil {
		return env, nil
	}
	if ptr.IsNil(provider) {
		return nil, errdefs.Validationf(
			"script bindings: provider is a typed nil %T", provider)
	}
	ordinary, err := provider.Bind(inv)
	if err != nil {
		return nil, err
	}
	if err := installBindings(env, ordinary); err != nil {
		return nil, err
	}
	late, ok := provider.(LateProvider)
	if !ok {
		observeSurface(inv, env)
		return env, nil
	}
	extra, err := late.BindLate(inv, env)
	if err != nil {
		return nil, err
	}
	if err := installBindings(env, extra); err != nil {
		return nil, err
	}
	observeSurface(inv, env)
	return env, nil
}

// installBindings writes bindings into env, rejecting empty names and
// duplicates, and names a script could not reference as an identifier.
func installBindings(env *agent.ScriptEnv, list []Binding) error {
	for _, b := range list {
		if b.Name == "" {
			return errdefs.Validationf("script bindings: provider returned an empty global name")
		}
		if !validGlobalName(b.Name) {
			return errdefs.Validationf(
				"script bindings: %q is not a usable global name: it must be a "+
					"JavaScript/Lua identifier that is not a keyword", b.Name)
		}
		if _, dup := env.Bindings[b.Name]; dup {
			return errdefs.Validationf(
				"script bindings: global %q is bound more than once", b.Name)
		}
		env.Bindings[b.Name] = b.Value
	}
	return nil
}

// observeSurface records what one execution's script scope carries: the
// assembled globals are otherwise invisible — a script failing on a
// missing global looks exactly like a script failing on a typo, and an
// over-wide surface has no other trace.
//
// The report rides the caller's span (the graph node span, in the script
// node path) and the debug log; both are no-ops when telemetry is
// disabled.
func observeSurface(inv Invocation, env *agent.ScriptEnv) {
	span := trace.SpanFromContext(inv.Context)
	span.SetAttributes(attribute.Int("script.bindings.count", len(env.Bindings)))
	if !telemetry.DebugEnabled(inv.Context) {
		return
	}
	names := sortedBindingNames(env)
	telemetry.Debug(inv.Context, "script bindings assembled",
		otellog.String(telemetry.AttrNodeID, inv.NodeID),
		otellog.String("script.bindings.node_type", inv.NodeType),
		otellog.Int("script.bindings.count", len(names)),
		otellog.String("script.bindings.names", strings.Join(names, ",")),
	)
}

// sortedBindingNames returns the environment's global names in
// deterministic order.
func sortedBindingNames(env *agent.ScriptEnv) []string {
	if env == nil {
		return nil
	}
	names := make([]string, 0, len(env.Bindings))
	for name := range env.Bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
