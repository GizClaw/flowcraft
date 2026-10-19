package bindings

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// ordinaryOnly adapts a fixed binding list to a [Provider].
func ordinaryOnly(list ...Binding) Provider {
	return ProviderFunc(func(Invocation) ([]Binding, error) { return list, nil })
}

// bindingNames lists bindings in the order a merge returned them, so a
// test can pin the order rather than the set.
func bindingNames(list []Binding) []string {
	names := make([]string, 0, len(list))
	for _, b := range list {
		names = append(names, b.Name)
	}
	return names
}

// loggedPart is a two-phase stub that logs its ordinary phase, so a test
// can pin the order a merge calls its parts in.
type loggedPart struct {
	name string
	log  *[]string
	providerWithLate
}

func (p loggedPart) Bind(inv Invocation) ([]Binding, error) {
	*p.log = append(*p.log, p.name+".bind")
	return p.providerWithLate.Bind(inv)
}

func (p loggedPart) BindLate(inv Invocation, env *agent.ScriptEnv) ([]Binding, error) {
	return p.providerWithLate.BindLate(inv, env)
}

func TestMergeRunsPartsInOrder(t *testing.T) {
	var log []string
	first := loggedPart{name: "first", log: &log, providerWithLate: providerWithLate{
		ordinary: []Binding{{Name: "a", Value: 1}},
		late: func(env *agent.ScriptEnv) ([]Binding, error) {
			log = append(log, "first.late")
			if env.Bindings["a"] != 1 || env.Bindings["b"] != 2 {
				return nil, errors.New("late phase did not see the ordinary bindings")
			}
			return []Binding{{Name: "a_late", Value: 3}}, nil
		},
	}}
	second := loggedPart{name: "second", log: &log, providerWithLate: providerWithLate{
		ordinary: []Binding{{Name: "b", Value: 2}},
		late: func(*agent.ScriptEnv) ([]Binding, error) {
			log = append(log, "second.late")
			return []Binding{{Name: "b_late", Value: 4}}, nil
		},
	}}

	// The unlabeled nil part contributes nothing and is skipped.
	merged := Merge(
		Part{Owner: `dep "first"`, Provider: first},
		Part{},
		Part{Owner: `dep "second"`, Provider: second},
	)
	env, err := Assemble(testInvocation(), nil, merged)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if got, want := sortedBindingNames(env), []string{"a", "a_late", "b", "b_late"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("globals = %v, want %v", got, want)
	}
	wantLog := []string{"first.bind", "second.bind", "first.late", "second.late"}
	if !reflect.DeepEqual(log, wantLog) {
		t.Fatalf("call order = %v, want %v", log, wantLog)
	}
}

func TestMergeAttributesPartErrors(t *testing.T) {
	boom := errdefs.NotFoundf("widget missing")

	t.Run("labeled", func(t *testing.T) {
		merged := Merge(Part{Owner: `dep "unit.a"`, Provider: ProviderFunc(
			func(Invocation) ([]Binding, error) { return nil, boom })})
		_, err := Assemble(testInvocation(), nil, merged)
		if !errdefs.IsNotFound(err) {
			t.Fatalf("Assemble = %v, want the part's NotFound to survive", err)
		}
		for _, want := range []string{`dep "unit.a"`, "widget missing"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %v must contain %s", err, want)
			}
		}
	})

	t.Run("labeled late phase", func(t *testing.T) {
		late := providerWithLate{late: func(*agent.ScriptEnv) ([]Binding, error) { return nil, boom }}
		_, err := Assemble(testInvocation(), nil,
			Merge(Part{Owner: `dep "unit.l"`, Provider: late}))
		if !errdefs.IsNotFound(err) {
			t.Fatalf("Assemble = %v, want the part's NotFound to survive", err)
		}
		if !strings.Contains(err.Error(), `dep "unit.l"`) {
			t.Fatalf("error %v must name the late part", err)
		}
	})

	t.Run("unlabeled", func(t *testing.T) {
		merged := Merge(Part{Provider: ProviderFunc(
			func(Invocation) ([]Binding, error) { return nil, boom })})
		_, err := Assemble(testInvocation(), nil, merged)
		if !errors.Is(err, boom) || err.Error() != boom.Error() {
			t.Fatalf("error %v, want the part's error left untouched: %v", err, boom)
		}
	})
}

func TestMergeRejectsTypedNilPart(t *testing.T) {
	var nilProvider *typedNilProvider
	merged := Merge(Part{Owner: `dep "unit.a"`, Provider: nilProvider})
	_, err := Assemble(testInvocation(), nil, merged)
	if !errdefs.IsValidation(err) {
		t.Fatalf("Assemble = %v, want Validation for a typed-nil part", err)
	}
	for _, want := range []string{`dep "unit.a"`, "typed nil"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v must contain %s", err, want)
		}
	}
}

func TestMergeRejectsDuplicateGlobals(t *testing.T) {
	cases := map[string]struct {
		parts []Part
		want  string
	}{
		"within one part": {
			parts: []Part{{Owner: `dep "unit.a"`, Provider: ordinaryOnly(
				Binding{Name: "dup", Value: 1}, Binding{Name: "dup", Value: 2})}},
			want: `global "dup" is bound twice by dep "unit.a"`,
		},
		"across parts": {
			parts: []Part{
				{Owner: `dep "unit.a"`, Provider: ordinaryOnly(Binding{Name: "dup", Value: 1})},
				{Owner: `dep "unit.b"`, Provider: ordinaryOnly(Binding{Name: "dup", Value: 2})},
			},
			want: `global "dup" bound by both dep "unit.a" and dep "unit.b"`,
		},
		"unlabeled parts": {
			parts: []Part{{Provider: ProviderFunc(func(Invocation) ([]Binding, error) {
				return []Binding{{Name: "dup", Value: 1}, {Name: "dup", Value: 2}}, nil
			})}},
			want: `global "dup" is bound more than once`,
		},
		"a labeled part and an unlabeled one": {
			parts: []Part{
				{Owner: `dep "unit.b"`, Provider: ordinaryOnly(Binding{Name: "dup", Value: 2})},
				{Provider: ordinaryOnly(Binding{Name: "dup", Value: 1})},
			},
			want: `global "dup" bound by both dep "unit.b" and an unlabeled part`,
		},
		"late phase against the ordinary one": {
			parts: []Part{{Owner: `dep "unit.l"`, Provider: providerWithLate{
				ordinary: []Binding{{Name: "dup", Value: 1}},
				late:     func(*agent.ScriptEnv) ([]Binding, error) { return []Binding{{Name: "dup", Value: 2}}, nil },
			}}},
			want: `global "dup" bound by dep "unit.l" was already bound before the late phase`,
		},
		"late phase against an earlier late phase": {
			parts: []Part{
				{Owner: `dep "unit.a"`, Provider: providerWithLate{
					late: func(*agent.ScriptEnv) ([]Binding, error) { return []Binding{{Name: "dup", Value: 1}}, nil }}},
				{Owner: `dep "unit.b"`, Provider: providerWithLate{
					late: func(*agent.ScriptEnv) ([]Binding, error) { return []Binding{{Name: "dup", Value: 2}}, nil }}},
			},
			want: `global "dup" bound by both dep "unit.a" and dep "unit.b"`,
		},
		"unlabeled late phase against the ordinary one": {
			parts: []Part{{Provider: providerWithLate{
				ordinary: []Binding{{Name: "dup", Value: 1}},
				late:     func(*agent.ScriptEnv) ([]Binding, error) { return []Binding{{Name: "dup", Value: 2}}, nil },
			}}},
			want: `global "dup" is bound again in the late phase`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Assemble(testInvocation(), nil, Merge(tc.parts...))
			if !errdefs.IsValidation(err) {
				t.Fatalf("Assemble = %v, want Validation", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v must contain %s", err, tc.want)
			}
		})
	}
}

func TestMergeRejectsUnusableGlobalNames(t *testing.T) {
	// A bad name is rejected as a name, even when two parts bind it: the
	// merge validates before it records, the rule Assemble applies.
	unwanted := []string{"bound more than once", "bound by both", "bound twice"}
	cases := map[string]struct {
		part   Part
		want   string
		absent []string
	}{
		"unlabeled": {
			part:   Part{Provider: ordinaryOnly(Binding{Name: "tokens.estimate", Value: 1})},
			want:   `"tokens.estimate" is not a usable global name`,
			absent: unwanted,
		},
		"labeled": {
			part: Part{Owner: `dep "unit.a"`, Provider: ordinaryOnly(
				Binding{Name: "tokens.estimate", Value: 1})},
			want:   `dep "unit.a": "tokens.estimate" is not a usable global name`,
			absent: unwanted,
		},
		"labeled and bound twice": {
			part: Part{Owner: `dep "unit.a"`, Provider: ordinaryOnly(
				Binding{Name: "tokens.estimate", Value: 1},
				Binding{Name: "tokens.estimate", Value: 2})},
			want:   `dep "unit.a": "tokens.estimate" is not a usable global name`,
			absent: unwanted,
		},
		"empty name": {
			part: Part{Owner: `dep "unit.a"`, Provider: ordinaryOnly(Binding{Value: 1})},
			want: `dep "unit.a": provider returned an empty global name`,
		},
		"late phase": {
			part: Part{Owner: `dep "unit.l"`, Provider: providerWithLate{
				late: func(*agent.ScriptEnv) ([]Binding, error) {
					return []Binding{{Value: 1}}, nil
				}}},
			want: `dep "unit.l": provider returned an empty global name`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Assemble(testInvocation(), nil, Merge(tc.part))
			if !errdefs.IsValidation(err) {
				t.Fatalf("Assemble = %v, want Validation", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v must contain %s", err, tc.want)
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(err.Error(), unwanted) {
					t.Fatalf("error %v must report the name, not the duplicate %q", err, unwanted)
				}
			}
		})
	}
}

func TestMergeBindsInPartOrder(t *testing.T) {
	list, err := Merge(
		Part{Owner: `dep "unit.b"`, Provider: ordinaryOnly(
			Binding{Name: "b_one", Value: 1}, Binding{Name: "b_two", Value: 2})},
		Part{Owner: `dep "unit.a"`, Provider: ordinaryOnly(Binding{Name: "a", Value: 3})},
	).Bind(testInvocation())
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// Part order, and each part's own order within it: the merge
	// concatenates, it does not sort.
	if got, want := bindingNames(list), []string{"b_one", "b_two", "a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bind order = %v, want %v", got, want)
	}
}

func TestMergeOfNothingBindsNothing(t *testing.T) {
	list, err := Merge().Bind(testInvocation())
	if err != nil || len(list) != 0 {
		t.Fatalf("Bind = %v, %v; want no bindings and no error", list, err)
	}
	env, err := Assemble(testInvocation(), nil, Merge())
	if err != nil || len(env.Bindings) != 0 {
		t.Fatalf("Assemble = %v, %v; want an empty surface", env, err)
	}
}
