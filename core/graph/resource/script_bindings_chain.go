package resource

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

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
// dep keys that bound it. The composition itself is [bindings.Merge],
// which the chain labels with the dep keys that mounted each provider:
// the attribution rules live in one place instead of a second
// implementation of the merge.
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
	parts, err := chainParts(in)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, errdefs.Validationf(
			"script bindings: no %q or %q dep: mount at least one surface, or use impl %q to bind nothing",
			chainBaseDep, chainUnitDep, NoScriptBindingsImpl)
	}
	return bindings.Merge(parts...), nil
}

// chainParts resolves the chain's deps into the parts of the merged
// surface, in binding order: "base" first, then every "unit" dep in
// sorted key order — "base" < "unit" < "unit.a" < "unit.b" under plain
// string ordering, which is what puts the bare "unit" key in its
// documented middle position.
//
// Each part carries the dep key that mounted it, written the way the
// merge's messages should read ("dep \"unit.a\""), so a conflict names
// what collided rather than a position in the chain. A dep that is nil,
// not a provider or a typed nil is rejected here; so is the same
// provider mounted under two keys, where chainProviderIdentity can tell
// the two apart. That check is best-effort: a doubly mounted provider
// the build cannot identify is caught by the merge's duplicate-name
// check at execution time instead, which names both dep keys.
func chainParts(in res.Input) ([]bindings.Part, error) {
	keys := make([]string, 0, len(in.Deps))
	for key := range in.Deps {
		if key == chainBaseDep || key == chainUnitDep || strings.HasPrefix(key, chainUnitDep+".") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	out := make([]bindings.Part, 0, len(keys))
	mounted := make(map[string]string) // provider identity -> dep key
	for _, key := range keys {
		value := in.Deps[key]
		if value == nil {
			return nil, errdefs.Validationf("script bindings: dep %q is nil", key)
		}
		surface, ok := value.(bindings.Provider)
		if !ok {
			return nil, errdefs.Validationf(
				"script bindings: dep %q is %T, want a bindings.Provider", key, value)
		}
		if ptr.IsNil(surface) {
			return nil, errdefs.Validationf("script bindings: dep %q is a typed nil", key)
		}
		if identity, has := chainProviderIdentity(value); has {
			if first, dup := mounted[identity]; dup {
				return nil, errdefs.Conflictf(
					"script bindings: provider mounted twice (as %q and %q)", first, key)
			}
			mounted[identity] = key
		}
		out = append(out, bindings.Part{Owner: chainDepOwner(key), Provider: surface})
	}
	return out, nil
}

// chainDepOwner renders a dep key as its part's owner label. The merge
// prints a label verbatim, so the label names itself a dep and the
// message reads "dep \"unit.a\": ..." wherever the merge attributes.
func chainDepOwner(key string) string { return fmt.Sprintf("dep %q", key) }

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
