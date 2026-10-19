package resource

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/errdefs"
	res "github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// ChainImpl is the impl name of the provider that composes several
// "agent.ScriptBindings" resources into one surface: the optional
// "base" dep binds first, then every "unit" dep in sorted key order,
// each unit's late phase following the same order.
//
// Composition expressed as a resource stays visible to the deployment:
// it rides the resource DAG, is built once, can be shared by several
// engines, and its failures name the dep they came from — a unit whose
// Bind fails is reported with its dep key, a duplicate global with the
// dep keys that bound it.
//
//	resources:
//	  panel:
//	    kind: agent.ScriptBindings
//	    impl: chain
//	    deps: {base: std, unit.audit: audit, unit.tokens: tokens}
//
// A unit should bind its own globals only: the base is already in the
// chain, and a name bound twice fails the execution instead of
// shadowing.
//
// A chain composes what it is given, so it needs at least one dep: a
// document that writes "impl: chain" and forgets them fails the build
// instead of deploying a surface that binds nothing, because that is
// what "impl: none" already says explicitly.
const ChainImpl = "chain"

// Dep names of the chain resource.
const (
	chainBaseDep = "base"
	chainUnitDep = "unit"
)

// ScriptBindingsChainFactory builds the "agent.ScriptBindings/chain"
// resource.
type ScriptBindingsChainFactory struct{}

// ScriptBindingsChainSettings is the strict settings subtree of the
// chain resource. It is deliberately empty — composition is expressed
// by deps — so a settings key written by mistake fails the build
// instead of being silently ignored.
type ScriptBindingsChainSettings struct{}

// Spec implements res.Factory.
func (ScriptBindingsChainFactory) Spec() res.Spec {
	return res.Spec{
		Kind: bindings.ResourceKind,
		Impl: ChainImpl,
		Deps: []res.DepSpec{
			{Name: chainBaseDep, Type: bindings.ResourceKind},
			{Name: chainUnitDep, Type: bindings.ResourceKind, Many: true},
		},
	}
}

// New implements res.Factory.
func (ScriptBindingsChainFactory) New(ctx context.Context, in res.Input) (any, error) {
	if _, err := res.DecodeTyped[ScriptBindingsChainSettings](ctx, in.Settings); err != nil {
		return nil, err
	}
	units, err := chainUnits(in)
	if err != nil {
		return nil, err
	}
	if len(units) == 0 {
		return nil, errdefs.Validationf(
			"script bindings chain: no %q or %q dep: mount at least one surface, or use impl %q to bind nothing",
			chainBaseDep, chainUnitDep, NoScriptBindingsImpl)
	}
	return chainProvider{units: units}, nil
}

// chainUnit is one surface of the composed chain together with the dep
// key that mounted it, kept for error attribution.
type chainUnit struct {
	dep     string
	surface bindings.Provider
}

// chainUnits resolves the chain's deps in binding order: "base" first,
// then every "unit" dep in sorted key order — "base" < "unit" <
// "unit.a" < "unit.b" under plain string ordering, which is what puts
// the bare "unit" key in its documented middle position. A dep that is
// nil, not a provider or a typed nil is rejected here; so is the same
// provider mounted under two keys, where chainProviderIdentity can tell
// the two apart. That check is best-effort: a doubly mounted provider
// the build cannot identify is caught by the duplicate-name check at
// execution time instead, which names both dep keys.
func chainUnits(in res.Input) ([]chainUnit, error) {
	keys := make([]string, 0, len(in.Deps))
	for key := range in.Deps {
		if key == chainBaseDep || key == chainUnitDep || strings.HasPrefix(key, chainUnitDep+".") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	out := make([]chainUnit, 0, len(keys))
	mounted := make(map[string]string) // provider identity -> dep key
	for _, key := range keys {
		value := in.Deps[key]
		if value == nil {
			return nil, errdefs.Validationf("script bindings chain: dep %q is nil", key)
		}
		surface, ok := value.(bindings.Provider)
		if !ok {
			return nil, errdefs.Validationf(
				"script bindings chain: dep %q is %T, want a bindings.Provider", key, value)
		}
		if ptr.IsNil(surface) {
			return nil, errdefs.Validationf("script bindings chain: dep %q is a typed nil", key)
		}
		if identity, has := chainProviderIdentity(value); has {
			if first, dup := mounted[identity]; dup {
				return nil, errdefs.Conflictf(
					"script bindings chain: provider mounted twice (as %q and %q)", first, key)
			}
			mounted[identity] = key
		}
		out = append(out, chainUnit{dep: key, surface: surface})
	}
	return out, nil
}

// chainProviderIdentity returns a stable identity for pointer-backed
// provider values. It follows registrarIdentity but is narrower on
// purpose: functions are excluded because reflection only exposes their
// code pointer, so two independent ProviderFunc resources (one per
// "impl: none", say) would collapse into one identity, and a zero
// pointer is no identity at all.
//
// Everything else is identity-free too, the kind's own impls included:
// "standard" and "chain" return struct values, "none" a function, so
// mounting one of them under two keys is caught only when the duplicate
// global is bound, never at build time. Identity is pointer equality
// rather than value equality, so two resources that share one
// pointer-backed value are refused as well, and a value with no storage
// of its own — a pointer to a zero-size type, an empty slice — can read
// as the identity of an unrelated one.
func chainProviderIdentity(value any) (string, bool) {
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		if rv.Pointer() == 0 {
			return "", false
		}
		return fmt.Sprintf("%s:%d", rv.Type(), rv.Pointer()), true
	default:
		return "", false
	}
}

// chainProvider is the composed surface: ordinary bindings run unit by
// unit in order, then each unit's late phase in the same order.
//
// It mirrors bindings.Chain, but merges the bindings itself instead of
// delegating, so a duplicate name can be reported with the dep keys
// that bound it. The two implementations must stay in step; the parity
// test in script_bindings_chain_test.go pins the behaviour.
type chainProvider struct {
	units []chainUnit
}

// Bind implements [bindings.Provider]. The chain merges what the units
// return itself, so a duplicate is reported with the dep keys that bound
// it instead of by [bindings.Assemble] — which also means the merge
// decides a colliding name's error: two units binding an empty or
// otherwise unusable name report a duplicate, while one alone reports
// Assemble's name error.
func (c chainProvider) Bind(inv bindings.Invocation) ([]bindings.Binding, error) {
	out := make([]bindings.Binding, 0)
	owner := make(map[string]string)
	for _, unit := range c.units {
		list, err := unit.surface.Bind(inv)
		if err != nil {
			return nil, errdefs.Fmt("script bindings chain: dep %q: %w", unit.dep, err)
		}
		for _, b := range list {
			if first, dup := owner[b.Name]; dup {
				return nil, chainDuplicatef(b.Name, first, unit.dep)
			}
			owner[b.Name] = unit.dep
			out = append(out, b)
		}
	}
	return out, nil
}

// BindLate implements [bindings.LateProvider]: every unit that has a
// late phase runs it in binding order, against the environment the
// ordinary bindings built. Assemble installs the returned bindings
// after this call, so a late binding does not see its siblings — the
// same contract as bindings.Chain.
func (c chainProvider) BindLate(inv bindings.Invocation, env *agent.ScriptEnv) ([]bindings.Binding, error) {
	out := make([]bindings.Binding, 0)
	owner := make(map[string]string, len(env.Bindings))
	for name := range env.Bindings {
		owner[name] = ""
	}
	for _, unit := range c.units {
		late, ok := unit.surface.(bindings.LateProvider)
		if !ok {
			continue
		}
		list, err := late.BindLate(inv, env)
		if err != nil {
			return nil, errdefs.Fmt("script bindings chain: dep %q: %w", unit.dep, err)
		}
		for _, b := range list {
			first, dup := owner[b.Name]
			switch {
			case !dup:
			case first == "":
				// Bound before this late phase — by the chain's own
				// ordinary bindings or by an enclosing provider — so the
				// chain cannot name the dep that bound it.
				return nil, errdefs.Validationf(
					"script bindings chain: global %q bound by dep %q was already bound in the ordinary phase",
					b.Name, unit.dep)
			default:
				return nil, chainDuplicatef(b.Name, first, unit.dep)
			}
			owner[b.Name] = unit.dep
			out = append(out, b)
		}
	}
	return out, nil
}

// chainDuplicatef reports a global bound by two units, naming both.
// A name bound twice by the same unit reads differently from one bound
// by two: the later phrasing would otherwise name the same dep twice.
func chainDuplicatef(name, first, second string) error {
	if first == second {
		return errdefs.Validationf(
			"script bindings chain: global %q is bound twice by dep %q", name, first)
	}
	return errdefs.Validationf(
		"script bindings chain: global %q bound by both %q and %q", name, first, second)
}
