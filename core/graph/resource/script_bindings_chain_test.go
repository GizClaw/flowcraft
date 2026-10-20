package resource_test

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/errdefs"
	graphresource "github.com/GizClaw/flowcraft/core/graph/resource"
	"github.com/GizClaw/flowcraft/core/resource"
)

// chainStub is a script surface stub with both phases: it records the
// calls it receives into a shared log, notes the globals visible to its
// late phase, and returns the bindings it was built with.
type chainStub struct {
	name    string
	log     *[]string
	ord     []bindings.Binding
	late    []bindings.Binding
	lateAt  *[]string
	ordErr  error
	lateErr error
}

func (s *chainStub) Bind(bindings.Invocation) ([]bindings.Binding, error) {
	s.record(".bind")
	if s.ordErr != nil {
		return nil, s.ordErr
	}
	return append([]bindings.Binding(nil), s.ord...), nil
}

func (s *chainStub) BindLate(_ bindings.Invocation, env *agent.ScriptEnv) ([]bindings.Binding, error) {
	s.record(".late")
	if s.lateAt != nil {
		names := make([]string, 0, len(env.Bindings))
		for name := range env.Bindings {
			names = append(names, name)
		}
		sort.Strings(names)
		*s.lateAt = append(*s.lateAt, strings.Join(names, ","))
	}
	if s.lateErr != nil {
		return nil, s.lateErr
	}
	return append([]bindings.Binding(nil), s.late...), nil
}

func (s *chainStub) record(suffix string) {
	if s.log != nil {
		*s.log = append(*s.log, s.name+suffix)
	}
}

// plainStub is a script surface stub without a late phase.
type plainStub struct {
	name string
	log  *[]string
	ord  []bindings.Binding
}

func (s plainStub) Bind(bindings.Invocation) ([]bindings.Binding, error) {
	if s.log != nil {
		*s.log = append(*s.log, s.name+".bind")
	}
	return append([]bindings.Binding(nil), s.ord...), nil
}

func buildChain(t *testing.T, deps map[string]any) bindings.Provider {
	t.Helper()
	value, err := (graphresource.ScriptBindingsChainFactory{}).New(
		context.Background(), resource.NewTestInput(deps))
	if err != nil {
		t.Fatalf("chain New: %v", err)
	}
	provider := value
	return provider
}

func chainInvocation() bindings.Invocation {
	return bindings.Invocation{
		Context: context.Background(),
		Board:   agent.NewBoard(),
		Host:    agent.NoopHost{},
		RunInfo: agent.RunInfo{Identity: agent.Identity{AgentID: "a", RunID: "run-1"}},
		Runtime: scriptRuntimeStub{},
	}
}

func assembleChain(t *testing.T, provider bindings.Provider) *agent.ScriptEnv {
	t.Helper()
	env, err := bindings.Assemble(chainInvocation(), nil, provider)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return env
}

func envNames(env *agent.ScriptEnv) []string {
	names := make([]string, 0, len(env.Bindings))
	for name := range env.Bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func bindingNames(list []bindings.Binding) []string {
	names := make([]string, 0, len(list))
	for _, b := range list {
		names = append(names, b.Name)
	}
	return names
}

func TestScriptBindingsChainOrder(t *testing.T) {
	var log []string
	base := &chainStub{name: "base", log: &log, ord: []bindings.Binding{{Name: "board", Value: "b"}}}
	unitA := &chainStub{name: "a", log: &log, ord: []bindings.Binding{
		{Name: "a_one", Value: "1"},
		{Name: "a_two", Value: "2"},
	}}
	unitB := plainStub{name: "b", log: &log, ord: []bindings.Binding{{Name: "b", Value: "3"}}}

	// The map literal lists "unit.b" before "unit.a": binding order is
	// the sorted dep key order, not the document's.
	provider := buildChain(t, map[string]any{"unit.b": unitB, "unit.a": unitA, "base": base})

	list, err := provider.Bind(chainInvocation())
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	want := []string{"board", "a_one", "a_two", "b"}
	if got := bindingNames(list); !reflect.DeepEqual(got, want) {
		t.Fatalf("bind order = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(log, []string{"base.bind", "a.bind", "b.bind"}) {
		t.Fatalf("call order = %v", log)
	}
}

func TestScriptBindingsChainBareUnitKeyOrder(t *testing.T) {
	var log []string
	base := &chainStub{name: "base", log: &log, ord: []bindings.Binding{{Name: "board", Value: "b"}}}
	bare := &chainStub{name: "bare", log: &log,
		ord:  []bindings.Binding{{Name: "bare_one", Value: "1"}},
		late: []bindings.Binding{{Name: "bare_late", Value: "1l"}},
	}
	unitA := &chainStub{name: "a", log: &log,
		ord:  []bindings.Binding{{Name: "a_one", Value: "2"}},
		late: []bindings.Binding{{Name: "a_late", Value: "2l"}},
	}

	// The bare "unit" key is the documented middle position: after
	// "base", before the "unit." prefixes — in both phases.
	provider := buildChain(t, map[string]any{
		"unit.a": unitA, "unit": bare, "base": base,
	})
	list, err := provider.Bind(chainInvocation())
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	wantBind := []string{"board", "bare_one", "a_one"}
	if got := bindingNames(list); !reflect.DeepEqual(got, wantBind) {
		t.Fatalf("bind order = %v, want %v", got, wantBind)
	}

	// Assemble binds the units again; drop the first pass's entries so
	// the late-phase order below comes from one call.
	log = log[:0]
	env := assembleChain(t, provider)
	wantNames := []string{"a_late", "a_one", "bare_late", "bare_one", "board"}
	if got := envNames(env); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("globals = %v, want %v", got, wantNames)
	}
	wantLog := []string{"base.bind", "bare.bind", "a.bind", "base.late", "bare.late", "a.late"}
	if !reflect.DeepEqual(log, wantLog) {
		t.Fatalf("call order = %v, want %v", log, wantLog)
	}
}

// structSurface is a provider whose dynamic type is a struct value,
// like the standardBindings and merged surfaces the kind itself builds.
type structSurface struct{ ord []bindings.Binding }

func (s structSurface) Bind(bindings.Invocation) ([]bindings.Binding, error) {
	return append([]bindings.Binding(nil), s.ord...), nil
}

func TestScriptBindingsChainIdentityIsBestEffort(t *testing.T) {
	// chainProviderIdentity only identifies pointer-backed values, so the
	// kind's own impls — "standard" and "chain" return structs, "none" a
	// function — never trip the build-time check: mounting one under two
	// keys builds, and the duplicate global is caught at execution time
	// naming both keys.
	shared := structSurface{ord: []bindings.Binding{{Name: "board", Value: "b"}}}
	provider := buildChain(t, map[string]any{"base": shared, "unit.same": shared})
	_, err := bindings.Assemble(chainInvocation(), nil, provider)
	if !errdefs.IsValidation(err) {
		t.Fatalf("Assemble = %v, want Validation for the duplicate global", err)
	}
	if !strings.Contains(err.Error(), `global "board" bound by both dep "base" and dep "unit.same"`) {
		t.Fatalf("error %v must name both dep keys", err)
	}
}

func TestScriptBindingsChainLatePhase(t *testing.T) {
	var log []string
	var lateAt []string
	base := &chainStub{name: "base", log: &log, ord: []bindings.Binding{{Name: "board", Value: "b"}}}
	unitA := &chainStub{name: "a", log: &log,
		ord:    []bindings.Binding{{Name: "a", Value: "1"}},
		late:   []bindings.Binding{{Name: "a_late", Value: "1l"}},
		lateAt: &lateAt,
	}
	unitB := plainStub{name: "b", log: &log, ord: []bindings.Binding{{Name: "b", Value: "2"}}}
	unitC := &chainStub{name: "c", log: &log, lateAt: &lateAt,
		late: []bindings.Binding{{Name: "c_late", Value: "3l"}},
	}

	provider := buildChain(t,
		map[string]any{"base": base, "unit.a": unitA, "unit.b": unitB, "unit.c": unitC})
	env := assembleChain(t, provider)

	wantNames := []string{"a", "a_late", "b", "board", "c_late"}
	if got := envNames(env); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("globals = %v, want %v", got, wantNames)
	}
	wantLog := []string{"base.bind", "a.bind", "b.bind", "c.bind", "base.late", "a.late", "c.late"}
	if !reflect.DeepEqual(log, wantLog) {
		t.Fatalf("call order = %v, want %v", log, wantLog)
	}
	// Each late phase sees the ordinary bindings — base, a and b — and
	// not a sibling's late binding: Assemble installs the late list only
	// after the whole phase returned. The stub without a late phase
	// (plainStub b) was skipped.
	wantLateAt := []string{"a,b,board", "a,b,board"}
	if !reflect.DeepEqual(lateAt, wantLateAt) {
		t.Fatalf("late phase saw %v, want %v", lateAt, wantLateAt)
	}
}

func TestScriptBindingsChainNests(t *testing.T) {
	inner := buildChain(t, map[string]any{
		"base":     &chainStub{name: "std", ord: []bindings.Binding{{Name: "board", Value: "b"}}},
		"unit.tok": &chainStub{name: "tok", ord: []bindings.Binding{{Name: "tokens", Value: "t"}}},
	})
	audit := &chainStub{name: "audit", late: []bindings.Binding{{Name: "audit", Value: "a"}}}
	outer := buildChain(t, map[string]any{"base": inner, "unit.audit": audit})

	want := []string{"audit", "board", "tokens"}
	if got := envNames(assembleChain(t, outer)); !reflect.DeepEqual(got, want) {
		t.Fatalf("nested globals = %v, want %v", got, want)
	}
}

func TestScriptBindingsChainRejectsNoDeps(t *testing.T) {
	// A chain composes what it is given: written with no deps it would
	// deploy a surface that binds nothing, which is what "impl: none"
	// says on purpose — so it fails the build instead.
	for name, deps := range map[string]map[string]any{"nil deps": nil, "empty deps": {}} {
		t.Run(name, func(t *testing.T) {
			_, err := (graphresource.ScriptBindingsChainFactory{}).New(
				context.Background(), resource.NewTestInput(deps))
			if !errdefs.IsValidation(err) {
				t.Fatalf("New = %v, want Validation for a chain that composes nothing", err)
			}
			if !strings.Contains(err.Error(), `impl "none"`) {
				t.Fatalf("error %v must point at the explicit empty surface", err)
			}
		})
	}
}

func TestScriptBindingsChainRejectsBadDeps(t *testing.T) {
	cases := map[string]struct {
		deps map[string]any
		want string
	}{
		"plain nil":      {map[string]any{"base": nil}, `dep "base" is nil`},
		"non provider":   {map[string]any{"unit.a": "nope"}, `dep "unit.a" is string, want bindings.Provider`},
		"typed nil":      {map[string]any{"unit.a": (*chainStub)(nil)}, `dep "unit.a" is a typed nil`},
		"typed nil base": {map[string]any{"base": (*chainStub)(nil)}, `dep "base" is a typed nil`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := (graphresource.ScriptBindingsChainFactory{}).New(
				context.Background(), resource.NewTestInput(tc.deps))
			if !errdefs.IsValidation(err) {
				t.Fatalf("New = %v, want Validation", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v must contain %s", err, tc.want)
			}
		})
	}
}

func TestScriptBindingsChainAttributesBadGlobalNames(t *testing.T) {
	// A unit that returns a name no script could reference fails the
	// execution like any other unit failure: the message names the dep,
	// not just the rule.
	t.Run("ordinary phase", func(t *testing.T) {
		unit := &chainStub{name: "a", ord: []bindings.Binding{{Name: "tokens.estimate"}}}
		_, err := bindings.Assemble(chainInvocation(), nil,
			buildChain(t, map[string]any{"unit.a": unit}))
		if !errdefs.IsValidation(err) {
			t.Fatalf("Assemble = %v, want Validation", err)
		}
		want := `script bindings: dep "unit.a": "tokens.estimate" is not a usable global name`
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v must contain %s", err, want)
		}
	})

	t.Run("late phase", func(t *testing.T) {
		unit := &chainStub{name: "l", late: []bindings.Binding{{Name: ""}}}
		_, err := bindings.Assemble(chainInvocation(), nil,
			buildChain(t, map[string]any{"unit.l": unit}))
		if !errdefs.IsValidation(err) {
			t.Fatalf("Assemble = %v, want Validation", err)
		}
		want := `script bindings: dep "unit.l": provider returned an empty global name`
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v must contain %s", err, want)
		}
	})
}

func TestScriptBindingsChainMinimalShapes(t *testing.T) {
	// Both deps are optional in the spec; only the pair together is
	// required to be non-empty.
	t.Run("base only", func(t *testing.T) {
		base := &chainStub{name: "base", ord: []bindings.Binding{{Name: "board", Value: "b"}}}
		env := assembleChain(t, buildChain(t, map[string]any{"base": base}))
		if got, want := envNames(env), []string{"board"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("globals = %v, want %v", got, want)
		}
	})

	t.Run("bare unit only", func(t *testing.T) {
		unit := &chainStub{name: "unit", ord: []bindings.Binding{{Name: "tokens", Value: "t"}}}
		env := assembleChain(t, buildChain(t, map[string]any{"unit": unit}))
		if got, want := envNames(env), []string{"tokens"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("globals = %v, want %v", got, want)
		}
	})
}

func TestScriptBindingsChainRejectsSameProviderUnderTwoKeys(t *testing.T) {
	stub := &chainStub{name: "shared", ord: []bindings.Binding{{Name: "x", Value: "1"}}}
	_, err := (graphresource.ScriptBindingsChainFactory{}).New(context.Background(), resource.NewTestInput(map[string]any{"unit.a": stub, "unit.b": stub}))
	if !errdefs.IsConflict(err) {
		t.Fatalf("New = %v, want Conflict", err)
	}
	for _, want := range []string{`"unit.a"`, `"unit.b"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v must name %s", err, want)
		}
	}
}

func TestScriptBindingsChainKeepsIndependentFunctionProviders(t *testing.T) {
	// Two "impl: none" resources and two wrappers of the same function
	// share a code pointer under reflection: treating functions as
	// identity-bearing would reject these legal documents.
	none := func() bindings.Provider {
		value, err := (graphresource.ScriptBindingsNoneFactory{}).New(context.Background(), resource.NewTestInput(nil))
		if err != nil {
			t.Fatalf("none New: %v", err)
		}
		provider := value
		return provider
	}
	same := func(bindings.Invocation) ([]bindings.Binding, error) { return nil, nil }
	provider := buildChain(t, map[string]any{
		"unit.a": none(),
		"unit.b": none(),
		"unit.c": bindings.ProviderFunc(same),
		"unit.d": bindings.ProviderFunc(same),
	})
	if env := assembleChain(t, provider); len(env.Bindings) != 0 {
		t.Fatalf("bindings = %v, want none", env.Bindings)
	}
}

func TestScriptBindingsChainDuplicateGlobals(t *testing.T) {
	t.Run("within one unit", func(t *testing.T) {
		unit := &chainStub{name: "a", ord: []bindings.Binding{
			{Name: "dup", Value: "1"},
			{Name: "dup", Value: "2"},
		}}
		_, err := bindings.Assemble(chainInvocation(), nil,
			buildChain(t, map[string]any{"unit.a": unit}))
		if !errdefs.IsValidation(err) {
			t.Fatalf("Assemble = %v, want Validation", err)
		}
		if !strings.Contains(err.Error(), `global "dup" is bound twice by dep "unit.a"`) {
			t.Fatalf("error %v must name the unit that bound twice", err)
		}
	})

	t.Run("across units", func(t *testing.T) {
		provider := buildChain(t, map[string]any{
			"unit.a": &chainStub{name: "a", ord: []bindings.Binding{{Name: "dup", Value: "1"}}},
			"unit.b": &chainStub{name: "b", ord: []bindings.Binding{{Name: "dup", Value: "2"}}},
		})
		_, err := bindings.Assemble(chainInvocation(), nil, provider)
		if !errdefs.IsValidation(err) {
			t.Fatalf("Assemble = %v, want Validation", err)
		}
		if !strings.Contains(err.Error(), `global "dup" bound by both dep "unit.a" and dep "unit.b"`) {
			t.Fatalf("error %v must name both deps", err)
		}
	})
}

func TestScriptBindingsChainAttributesUnitErrors(t *testing.T) {
	t.Run("ordinary phase", func(t *testing.T) {
		unit := &chainStub{name: "a", ordErr: errdefs.NotFoundf("widget missing")}
		_, err := bindings.Assemble(chainInvocation(), nil,
			buildChain(t, map[string]any{"unit.a": unit}))
		if !errdefs.IsNotFound(err) {
			t.Fatalf("Assemble = %v, want the unit's NotFound to survive", err)
		}
		if errdefs.IsValidation(err) {
			t.Fatalf("Assemble = %v, want the unit's classification inherited, not overridden", err)
		}
		if !strings.Contains(err.Error(), `dep "unit.a"`) || !strings.Contains(err.Error(), "widget missing") {
			t.Fatalf("error %v must name the dep and the unit's cause", err)
		}
	})

	t.Run("late phase", func(t *testing.T) {
		unit := &chainStub{name: "l", lateErr: errdefs.NotFoundf("late missing")}
		_, err := bindings.Assemble(chainInvocation(), nil,
			buildChain(t, map[string]any{"unit.l": unit}))
		if !errdefs.IsNotFound(err) {
			t.Fatalf("Assemble = %v, want the unit's NotFound to survive", err)
		}
		if !strings.Contains(err.Error(), `dep "unit.l"`) || !strings.Contains(err.Error(), "late missing") {
			t.Fatalf("error %v must name the dep and the unit's cause", err)
		}
	})
}

func TestScriptBindingsChainLateCollision(t *testing.T) {
	t.Run("with the ordinary phase", func(t *testing.T) {
		base := &chainStub{name: "base", ord: []bindings.Binding{{Name: "board", Value: "b"}}}
		late := &chainStub{name: "l", late: []bindings.Binding{{Name: "board", Value: "l"}}}
		_, err := bindings.Assemble(chainInvocation(), nil,
			buildChain(t, map[string]any{"base": base, "unit.l": late}))
		if !errdefs.IsValidation(err) {
			t.Fatalf("Assemble = %v, want Validation", err)
		}
		want := `global "board" bound by dep "unit.l" was already bound before the late phase`
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v must name the late dep", err)
		}
	})

	t.Run("with an earlier unit's late phase", func(t *testing.T) {
		first := &chainStub{name: "a", late: []bindings.Binding{{Name: "dup", Value: "1"}}}
		second := &chainStub{name: "b", late: []bindings.Binding{{Name: "dup", Value: "2"}}}
		_, err := bindings.Assemble(chainInvocation(), nil,
			buildChain(t, map[string]any{"unit.a": first, "unit.b": second}))
		if !errdefs.IsValidation(err) {
			t.Fatalf("Assemble = %v, want Validation", err)
		}
		want := `global "dup" bound by both dep "unit.a" and dep "unit.b"`
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v must name both late deps", err)
		}
	})

	t.Run("inside a nested chain", func(t *testing.T) {
		inner := buildChain(t, map[string]any{
			"unit.a": &chainStub{name: "a", late: []bindings.Binding{{Name: "dup", Value: "1"}}},
			"unit.b": &chainStub{name: "b", late: []bindings.Binding{{Name: "dup", Value: "2"}}},
		})
		_, err := bindings.Assemble(chainInvocation(), nil,
			buildChain(t, map[string]any{"unit.inner": inner}))
		if !errdefs.IsValidation(err) {
			t.Fatalf("Assemble = %v, want Validation", err)
		}
		// The outer chain prefixes the unit it called, and the package
		// prefix appears once: the inner merge's message already carries
		// it, so attribution does not stack it.
		want := `script bindings: dep "unit.inner": global "dup" bound by both dep "unit.a" and dep "unit.b"`
		if got := err.Error(); got != want {
			t.Fatalf("error = %q, want %q", got, want)
		}
	})
}

func TestScriptBindingsChainSettingsAreStrict(t *testing.T) {
	for name, settings := range map[string]string{
		"unknown key": `{"order": ["unit.a"]}`,
		"units key":   `{"units": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (graphresource.ScriptBindingsChainFactory{}).New(
				context.Background(), resource.NewInput([]byte(settings), nil, nil, nil))
			if !errdefs.IsValidation(err) {
				t.Fatalf("New = %v, want Validation for a settings key the chain does not define", err)
			}
		})
	}

	// A dep, since a chain with none is itself a build error.
	if _, err := (graphresource.ScriptBindingsChainFactory{}).New(
		context.Background(),
		resource.NewTestInput(map[string]any{"unit.a": &chainStub{name: "a"}})); err != nil {
		t.Fatalf("empty settings: %v", err)
	}
}

func TestScriptBindingsChainSpec(t *testing.T) {
	if graphresource.ChainImpl != "chain" {
		t.Fatalf("ChainImpl = %q, want %q", graphresource.ChainImpl, "chain")
	}
	spec := (graphresource.ScriptBindingsChainFactory{}).Spec()
	if err := spec.Validate(); err != nil {
		t.Fatalf("spec: %v", err)
	}
	if spec.Kind != bindings.ResourceKind || spec.Impl != graphresource.ChainImpl {
		t.Fatalf("spec = %s/%s, want %s/%s", spec.Kind, spec.Impl, bindings.ResourceKind, graphresource.ChainImpl)
	}
	want := []resource.Requirement{
		resource.Optional[bindings.Provider]("base").Spec(),
		resource.OptionalMany[bindings.Provider]("unit").Spec(),
	}
	if !reflect.DeepEqual(spec.Deps, want) {
		t.Fatalf("deps = %+v, want %+v", spec.Deps, want)
	}
}
