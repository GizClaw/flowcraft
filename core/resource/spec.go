package resource

import (
	"bytes"
	"context"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils"
)

// DepSpec declares one named dependency accepted by a build factory.
// Type names the expected contract (e.g. "sandbox.Runner") for
// documentation and validation; it is not resolved by this package.
type DepSpec struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	// Many declares a list dependency: the document supplies zero or
	// more deps whose keys equal Name or start with Name+"." (e.g.
	// "provider", "provider.openai", "provider.qwen"). Input.DepsMany
	// collects them in deterministic key order.
	Many bool `json:"many,omitempty"`
}

// Spec is the static declaration of one build factory: the unique
// (Kind, Impl) registry key and the named dependencies [Factory.New]
// expects in [Input.Deps].
type Spec struct {
	Kind     Kind      `json:"kind"`
	Impl     string    `json:"impl,omitempty"`
	Deps     []DepSpec `json:"deps,omitempty"`
	ItemType string    `json:"item_type,omitempty"`
}

// Clone returns a defensive copy: the returned spec shares no backing
// array with the receiver.
func (s Spec) Clone() Spec {
	s.Deps = append([]DepSpec(nil), s.Deps...)
	return s
}

// Validate checks the static invariants every factory spec must
// satisfy: a non-empty kind, named+typed deps, no duplicate names.
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
		if dep.Type == "" {
			return errdefs.Validationf(
				"resource factory spec %s/%s: dep %q type is empty",
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

// Factory builds one resource value from an [Input] of raw settings
// and already-built dependencies. Factories decode and strictly
// validate their own settings inside New. A value implementing
// io.Closer is closed by the assembly layer in reverse construction
// order.
type Factory interface {
	// Spec returns the static declaration for this factory.
	Spec() Spec

	// New builds one value from resolved settings and dependencies.
	New(ctx context.Context, in Input) (any, error)
}

// ItemResolver is implemented by container resources that expose named
// items, making "resource/item" refs resolvable. A workspace registry
// exposing its workspaces, or a sandbox registry exposing its runners,
// are examples.
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

// DeploymentBinder is implemented by resource values that need the
// fully assembled deployment after every resource and agent has been
// wired. The assembly layer passes its read-only deployment view after
// agents are bound; implementations type-assert it to their own
// minimal interface (e.g. delegation's agent directory).
type DeploymentBinder interface {
	BindDeployment(deployment any) error
}

// Input is the universal factory input: the factory-owned settings
// subtree as raw JSON plus already-built dependencies keyed by the
// names used in the document.
type Input struct {
	Settings []byte
	Deps     map[string]any
	// Loader materializes Source references in settings subtrees. It
	// is the deployment-level loader injected by the assembly layer;
	// nil means source resolution is unavailable.
	Loader *Loader
	// Secrets is the assembled secret resolver for typed Secret
	// values; nil when the deployment declares no secret stores.
	Secrets *SecretResolver
}

// Dep returns the named dependency.
func (in Input) Dep(name string) (any, bool) {
	v, ok := in.Deps[name]
	return v, ok
}

// DepsMany returns every dependency whose key equals name or starts
// with name+"." (the list form of a [DepSpec] with Many), in sorted
// key order so the result is deterministic regardless of map
// iteration order.
func (in Input) DepsMany(name string) []any {
	prefix := name + "."
	var keys []string
	for key := range in.Deps {
		if key == name || strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	values := make([]any, 0, len(keys))
	for _, key := range keys {
		values = append(values, in.Deps[key])
	}
	return values
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
// materializes it. Inline settings are returned unchanged; {"file":…}
// and {"embed":…} settings are resolved through the loader.
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
