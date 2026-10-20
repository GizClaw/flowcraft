package resource

import (
	"reflect"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// Requirement is one dependency a factory declares: the document key, the Go
// type the resolved value must satisfy, and its shape.
//
// Type is the machine-checkable contract. It is derived from a type parameter
// by [Require]/[Optional]/[RequireMany]/[OptionalMany] and is the same type
// the factory's [Dep] handle narrows to, so the declaration and the read
// cannot drift. The assembly layer checks every edge against it before any
// factory runs.
type Requirement struct {
	// Name is the dep key used by the deployment document: "tool" for a
	// single dep, the prefix of "tool.*" keys for a Many dep.
	Name string `json:"name"`
	// Type is the Go type every resolved value must satisfy.
	Type reflect.Type `json:"-"`
	// Required makes the absence of this dep a plan error.
	Required bool `json:"required,omitempty"`
	// Many collects every key equal to Name or starting with Name+"." in
	// sorted key order.
	Many bool `json:"many,omitempty"`
}

// Dep is the authored form of a [Requirement]: one value that both declares a
// dependency in a [Factory]'s [Spec] and reads it from an [Input].
//
//	var depTool = resource.RequireMany[tool.Source]("tool")
//
//	func (f) Spec() resource.Spec {
//		return resource.Spec{Kind: ..., Impl: ..., Deps: []resource.Requirement{depTool.Spec()}}
//	}
//
//	func (f) New(ctx context.Context, in resource.Input) (*tool.Assembly, error) {
//		sources, err := depTool.GetAll(in)
//		...
//	}
//
// Both halves derive from T, so a factory cannot declare one type and assert
// another.
type Dep[T any] struct{ requirement Requirement }

// Require declares a required single dependency of type T.
func Require[T any](name string) Dep[T] {
	return Dep[T]{requirement: newRequirement[T](name, true, false)}
}

// Optional declares a dependency that may be absent from the document.
func Optional[T any](name string) Dep[T] {
	return Dep[T]{requirement: newRequirement[T](name, false, false)}
}

// RequireMany declares a required list dependency: every dep key equal to
// name or starting with name+"." (e.g. "provider", "provider.openai").
func RequireMany[T any](name string) Dep[T] {
	return Dep[T]{requirement: newRequirement[T](name, true, true)}
}

// OptionalMany declares a list dependency that may be empty.
func OptionalMany[T any](name string) Dep[T] {
	return Dep[T]{requirement: newRequirement[T](name, false, true)}
}

func newRequirement[T any](name string, required, many bool) Requirement {
	return Requirement{Name: name, Type: reflect.TypeFor[T](), Required: required, Many: many}
}

// Name reports the dependency key this handle reads.
func (d Dep[T]) Name() string { return d.requirement.Name }

// Spec reports the declaration this handle contributes to a factory's
// [Spec].
func (d Dep[T]) Spec() Requirement { return d.requirement }

// Get reads the single dependency.
//
// A missing dep is a NotFound error even for an [Optional] handle: use
// [Dep.TryGet] when absence is meaningful, so an optional dependency never
// masquerades as a zero value. The plan normally rejects a missing required
// dep before the factory runs, so this is defence in depth.
func (d Dep[T]) Get(in Input) (T, error) {
	var zero T
	raw, ok := in.dep(d.requirement.Name)
	if !ok {
		return zero, errdefs.NotFoundf(
			"resource: dep %q is missing (%s)", d.requirement.Name, d.requirement.Type)
	}
	return d.narrow(d.requirement.Name, raw)
}

// TryGet is [Dep.Get] for an optional dependency: absent is reported by
// ok=false, not by an error.
func (d Dep[T]) TryGet(in Input) (value T, ok bool, err error) {
	raw, present := in.dep(d.requirement.Name)
	if !present {
		return value, false, nil
	}
	value, err = d.narrow(d.requirement.Name, raw)
	if err != nil {
		return value, false, err
	}
	return value, true, nil
}

// GetAll reads every dependency whose key equals the handle's name or starts
// with name+"." in sorted key order, so the result is deterministic.
//
// An empty result for a Required Many dep is a plan error; GetAll repeats the
// check so a value built outside the plan phase cannot silently end up with no
// dependencies at all.
func (d Dep[T]) GetAll(in Input) ([]T, error) {
	keys := in.depKeys(d.requirement.Name)
	if len(keys) == 0 && d.requirement.Required {
		return nil, errdefs.Validationf(
			"resource: required dep %q has no values (%s)",
			d.requirement.Name, d.requirement.Type)
	}
	values := make([]T, 0, len(keys))
	for _, key := range keys {
		raw, _ := in.dep(key)
		value, err := d.narrow(key, raw)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

// Named is one resolved list dependency together with the document key it was
// declared under.
type Named[T any] struct {
	// Name is the dep key from the deployment document ("provider.openai").
	Name string
	// Value is the resolved dependency.
	Value T
}

// GetAllNamed is [Dep.GetAll] with the document keys kept, for factories that
// report per-key errors or dedup by key.
func (d Dep[T]) GetAllNamed(in Input) ([]Named[T], error) {
	keys := in.depKeys(d.requirement.Name)
	if len(keys) == 0 && d.requirement.Required {
		return nil, errdefs.Validationf(
			"resource: required dep %q has no values (%s)",
			d.requirement.Name, d.requirement.Type)
	}
	values := make([]Named[T], 0, len(keys))
	for _, key := range keys {
		raw, _ := in.dep(key)
		value, err := d.narrow(key, raw)
		if err != nil {
			return nil, err
		}
		values = append(values, Named[T]{Name: key, Value: value})
	}
	return values, nil
}

// narrow is the only any-to-T conversion in this package. Every caller is
// reached through a Dep[T] whose [Requirement] the plan already checked
// against the producer's build type, so a failure here means a broken
// invariant rather than a document error.
//
// key is the document key the value came from, which for a Many dep is the
// specific "name.suffix" rather than the requirement name: a diagnostic has
// to point at the dep the author wrote.
func (d Dep[T]) narrow(key string, raw any) (T, error) {
	var zero T
	if raw == nil {
		return zero, errdefs.Validationf(
			"resource: dep %q is nil, want %s", key, d.requirement.Type)
	}
	if ptr.IsNil(raw) {
		return zero, errdefs.Validationf(
			"resource: dep %q is a typed nil (%T), want %s", key, raw, d.requirement.Type)
	}
	value, ok := raw.(T)
	if !ok {
		return zero, errdefs.Validationf(
			"resource: dep %q is %T, want %s", key, raw, d.requirement.Type)
	}
	return value, nil
}
