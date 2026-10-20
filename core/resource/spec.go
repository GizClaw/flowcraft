package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// Factory builds one resource value of type T from a deployment entry.
//
// T is the value's Go type, and it is authoritative: the registry records it
// as the entry's build type, so the assembly layer can check every dependency
// edge against it before any factory runs. A factory therefore never declares
// its output type as a string, and consumers never guess it.
type Factory[T any] interface {
	// Spec returns the static declaration for this factory: the (Kind, Impl)
	// registry key, the typed dependency requirements, and the item type this
	// resource exposes (if it is a container). Spec must not depend on the
	// deployment document.
	Spec() Spec

	// New builds one value from resolved settings and dependencies.
	//
	// Settings are decoded and strictly validated here. Dependencies are
	// reachable only through the [Dep] handles this factory declared in
	// [Spec]: an [Input] has no map-shaped accessor, so a factory cannot
	// read a dependency it did not declare. A value implementing
	// [io.Closer] is closed by the assembly layer in reverse construction
	// order.
	New(ctx context.Context, in Input) (T, error)
}

// Entry is the erased form of a [Factory] held by a [Registry].
//
// An Entry carries the build type declared by the [Factory] it came from, and
// only [Register] can put one in a registry ([Registry.add] is unexported), so
// every registered entry is trustworthy: [Entry.BuildType] is what makes a
// whole deployment type-checkable before a single resource is constructed.
type Entry interface {
	// Spec returns the factory's static declaration.
	Spec() Spec
	// BuildType is the Go type the factory returns: reflect.TypeFor[T]().
	BuildType() reflect.Type
	// Build constructs the value. The assembly layer calls it after the plan
	// has proved the value's type and its dependencies' types.
	Build(ctx context.Context, in Input) (any, error)
}

// entry adapts one Factory[T] to the erased Entry the registry stores.
type entry[T any] struct{ factory Factory[T] }

func (e entry[T]) Spec() Spec { return e.factory.Spec() }

func (e entry[T]) BuildType() reflect.Type { return reflect.TypeFor[T]() }

// Build is the module's only any-producing conversion: a Factory[T] returns a
// T, and the assembly layer records it in a heterogeneous table keyed by
// resource name. Consumers read it back through [Dep.Get] or
// [deploy.Resolve], both of which narrow to a known type.
func (e entry[T]) Build(ctx context.Context, in Input) (any, error) {
	value, err := e.factory.New(ctx, in)
	if err != nil {
		var zero T
		return zero, err
	}
	return value, nil
}

// Register adds f to r under its Spec's (Kind, Impl).
//
// It is a free function rather than a method because Go has no generic
// methods: only a function can introduce the type parameter that ties
// Factory[T] to the erased Entry a Registry stores. Every registration site
// reads resource.Register(r, factory).
func Register[T any](r *Registry, f Factory[T]) error {
	if r == nil {
		return errdefs.Validationf("resource registry: nil registry")
	}
	if ptr.IsNil(f) {
		return errdefs.Validationf("resource registry: nil factory")
	}
	return r.add(entry[T]{factory: f})
}

// Spec is the static declaration of one factory: the unique (Kind, Impl)
// registry key, the named and typed dependencies [Factory.New] reads through
// its [Dep] handles, and the item type a container resource exposes.
type Spec struct {
	Kind Kind   `json:"kind"`
	Impl string `json:"impl,omitempty"`
	// Deps lists the dependencies the factory reads. Name is the key the
	// deployment document uses; Type is the Go type every resolved value must
	// satisfy, and is the only place that contract is written.
	Deps []Requirement `json:"deps,omitempty"`
	// Items is the Go type this resource exposes through [ItemResolver] when
	// a dependency uses an "resource/item" ref. Nil means the resource is not
	// a container.
	Items reflect.Type `json:"-"`
}

// Clone returns a defensive copy: the returned spec shares no backing
// array with the receiver.
func (s Spec) Clone() Spec {
	s.Deps = append([]Requirement(nil), s.Deps...)
	return s
}

// Key is the registry identity of this spec.
func (s Spec) Key() Key { return Key{Kind: s.Kind, Impl: s.Impl} }

// Validate checks the static invariants every factory spec must satisfy: a
// non-empty kind, named and typed deps, and no duplicate dep names.
func (s Spec) Validate() error {
	if s.Kind == "" {
		return errdefs.Validationf("resource factory spec: kind is empty")
	}
	seen := make(map[string]struct{}, len(s.Deps))
	for i, dep := range s.Deps {
		if dep.Name == "" {
			return errdefs.Validationf(
				"resource factory spec %s/%s: deps[%d].name is empty",
				s.Kind, s.Impl, i)
		}
		if dep.Type == nil {
			return errdefs.Validationf(
				"resource factory spec %s/%s: dep %q has no Go type; "+
					"declare it with resource.Require/Optional instead of a bare Requirement",
				s.Kind, s.Impl, dep.Name)
		}
		if strings.HasSuffix(dep.Name, ".") {
			return errdefs.Validationf(
				"resource factory spec %s/%s: dep %q name must not end with '.'",
				s.Kind, s.Impl, dep.Name)
		}
		if _, dup := seen[dep.Name]; dup {
			return errdefs.Validationf(
				"resource factory spec %s/%s: duplicate dep %q",
				s.Kind, s.Impl, dep.Name)
		}
		seen[dep.Name] = struct{}{}
	}
	return nil
}

// DepSpecFor returns the requirement matching one document dep key: the exact
// name for a single dep, or the "name" prefix for a [Requirement.Many] dep
// ("provider" matches "provider.openai").
func (s Spec) DepSpecFor(key string) (Requirement, bool) {
	for _, dep := range s.Deps {
		if dep.Name == key {
			return dep, true
		}
		if dep.Many && strings.HasPrefix(key, dep.Name+".") {
			return dep, true
		}
	}
	return Requirement{}, false
}

// ItemResolver is implemented by container resources that expose named
// items, making "resource/item" refs resolvable. A workspace registry
// exposing its workspaces, or a sandbox registry exposing its runners,
// are examples.
//
// Item returns any because one container may expose several item types; the
// resolving dependency's [Requirement.Type] is what constrains the value, and
// the assembly layer checks it at plan time.
type ItemResolver interface {
	ResolveItem(item string) (any, bool)
}

// Wireable is implemented by resource values that need a post-build
// attachment step: observers attaching to buses, hooks subscribing to
// event streams, backends registering into hosts. Wire runs after
// every resource is constructed and never participates in the
// construction DAG, so an observed value can never depend on its
// observer.
type Wireable interface {
	Wire(ctx context.Context) error
}

// Input is the factory's view of one document entry: the factory-owned
// settings subtree as raw JSON plus the already-built dependencies it
// declared.
//
// Dependencies are deliberately not exposed as a map. The only way to read
// one is the typed [Dep] handle that appeared in the factory's own [Spec], so
// the declared contract and the value a factory reads cannot diverge.
type Input struct {
	// Settings is the factory-owned settings subtree, after source
	// materialization and reference expansion.
	Settings json.RawMessage
	// Loader materializes Source references in settings subtrees. It is the
	// deployment-level loader injected by the assembly layer; nil means
	// source resolution is unavailable.
	Loader *Loader
	// Secrets is the assembled secret resolver for typed Secret values; nil
	// when the deployment declares no secret stores.
	Secrets *SecretResolver

	deps map[string]any
}

// NewInput constructs a factory input. It is the assembly layer's
// constructor: factories receive an Input, they never build one.
func NewInput(
	settings json.RawMessage,
	deps map[string]any,
	loader *Loader,
	secrets *SecretResolver,
) Input {
	return Input{Settings: settings, deps: deps, Loader: loader, Secrets: secrets}
}

// NewTestInput builds an Input carrying only dependencies. Test doubles and
// the test-support packages (*test) receive an Input rather than constructing
// one, and Input.deps is unexported, so this is their constructor.
func NewTestInput(deps map[string]any) Input { return Input{deps: deps} }

// WithSettings returns a copy of in carrying settings. It exists so test
// doubles can build an Input in one expression.
func (in Input) WithSettings(settings json.RawMessage) Input {
	in.Settings = settings
	return in
}

// Dep returns the raw dependency stored under name. It is unexported on
// purpose; package-internal callers are the [Dep] handles and nothing else.
func (in Input) dep(name string) (any, bool) {
	v, ok := in.deps[name]
	return v, ok
}

// depKeys returns the dependency keys matching name or name+".", in sorted
// order so [Dep.GetAll] is deterministic regardless of map iteration order.
func (in Input) depKeys(name string) []string {
	prefix := name + "."
	keys := make([]string, 0, len(in.deps))
	for key := range in.deps {
		if key == name || strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// Resolve materializes a [Source] through the input's loader.
func (in Input) Resolve(ctx context.Context, src Source) ([]byte, error) {
	if in.Loader == nil {
		return nil, errdefs.Validationf(
			"resource: source resolution is not configured")
	}
	return in.Loader.Load(ctx, src)
}

// ResolveSettings interprets Settings as a whole-subtree [Source] and
// materializes it. Inline settings are returned unchanged; {"file":…} and
// {"embed":…} settings are resolved through the loader.
func (in Input) ResolveSettings(ctx context.Context) ([]byte, error) {
	src, err := ParseSource(in.Settings)
	if err != nil {
		return nil, err
	}
	if !src.IsRef() {
		return bytes.Clone(in.Settings), nil
	}
	data, err := in.Resolve(ctx, src)
	if err != nil {
		return nil, err
	}
	// Sub-documents may be YAML; convert to JSON for strict settings
	// decoding.
	return utils.ToJSON(data)
}
