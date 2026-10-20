package hooks

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

const (
	// ResourceKind is the deployment kind of a command hook runner.
	ResourceKind = "hooks.Runner"

	// ObserverResourceKind is the deployment kind of the subagent
	// observer.
	ObserverResourceKind = "hooks.SubagentObserver"

	// ImplLocal is the local implementation of both kinds: hook files
	// on disk, commands run on this host.
	ImplLocal = "local"

	// EventBusDep is the observer's event bus dependency.
	EventBusDep = "events"

	// RunnerDep is the observer's runner dependency.
	RunnerDep = "runner"
)

// SourceProvider contributes extra hook sources to a runner. A plugin
// host implements it; a host wires one either as a factory option
// ([WithSources]) or as the document dependency declared with
// [WithSourceDep].
type SourceProvider interface {
	// HookSources returns the sources this provider contributes. It is
	// called once per build, so the result follows the provider's
	// current state.
	HookSources() []ExtraSource
}

// Settings is the runner's settings subtree.
type Settings struct {
	// Path is the hooks.json file. A file that does not exist yields a
	// runner with no groups; a file that does not parse fails the
	// build.
	Path string `json:"path"`
}

// FactoryOption configures a runner factory with host-owned wiring the
// document cannot express.
type FactoryOption func(*factoryConfig)

type factoryConfig struct {
	provider  SourceProvider
	sourceDep string
	// source is the erased typed handle for the document dependency that
	// carries extra sources. It is built by [WithSourceDep] from the
	// provider's own Go type, so the spec and the read cannot disagree.
	source *sourceBinding
}

// sourceBinding is one [resource.Dep] handle erased for storage in a
// non-generic config: the declaration it contributes to the spec, plus the
// typed read.
type sourceBinding struct {
	spec resource.Requirement
	read func(resource.Input) (SourceProvider, bool, error)
}

// WithSources wires a provider — normally the plugin host — whose
// sources are loaded alongside the document's hooks.json on every
// build.
func WithSources(provider SourceProvider) FactoryOption {
	return func(cfg *factoryConfig) { cfg.provider = provider }
}

// WithSourceDep declares the document dependency that carries extra
// sources. name is the dependency key; T is the Go type the dependency must
// produce, so the declared contract comes from the type parameter instead of a
// hand-written string. A craft host passes its own host type:
//
//	deps:
//	  plugins: craft.pluginhost
//
// The dependency is optional and T must implement [SourceProvider], so a
// deployment without a plugin host simply has one fewer hook source.
func WithSourceDep[T SourceProvider](name string) FactoryOption {
	name = strings.TrimSpace(name)
	dep := resource.Optional[T](name)
	return func(cfg *factoryConfig) {
		cfg.sourceDep = name
		cfg.source = &sourceBinding{
			spec: dep.Spec(),
			read: func(in resource.Input) (SourceProvider, bool, error) {
				value, ok, err := dep.TryGet(in)
				if err != nil || !ok {
					return nil, ok, err
				}
				return SourceProvider(value), true, nil
			},
		}
	}
}

// NewFactory returns the deployment factory of a command hook runner.
// Its value is a *[Manager].
func NewFactory(options ...FactoryOption) resource.Factory[*Manager] {
	cfg := factoryConfig{}
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	return runnerFactory{cfg: cfg}
}

type runnerFactory struct{ cfg factoryConfig }

// Spec implements resource.Factory.
func (f runnerFactory) Spec() resource.Spec {
	spec := resource.Spec{Kind: ResourceKind, Impl: ImplLocal}
	if f.cfg.source != nil {
		spec.Deps = []resource.Requirement{f.cfg.source.spec}
	}
	return spec
}

// New implements resource.Factory. A missing hooks.json is not an
// error: a host that installs no hooks of its own still starts.
func (f runnerFactory) New(ctx context.Context, in resource.Input) (*Manager, error) {
	settings, err := resource.DecodeTyped[Settings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("hooks: decode settings: %w", err))
	}
	path := strings.TrimSpace(settings.Path)
	if path == "" {
		return nil, errdefs.Validationf("hooks: settings.path is required")
	}
	sources, err := f.sources(in)
	if err != nil {
		return nil, err
	}
	return LoadWithSources(ctx, path, sources)
}

// sources collects the extra sources of one build.
func (f runnerFactory) sources(in resource.Input) ([]ExtraSource, error) {
	var sources []ExtraSource
	if !ptr.IsNil(f.cfg.provider) {
		sources = append(sources, f.cfg.provider.HookSources()...)
	}
	if f.cfg.sourceDep == "" {
		return sources, nil
	}
	provider, ok, err := f.cfg.source.read(in)
	if err != nil {
		return nil, err
	}
	if !ok {
		return sources, nil
	}
	return append(sources, provider.HookSources()...), nil
}

// depEventBus and depRunner are the observer's typed dependency handles.
var (
	depEventBus = resource.Require[event.Bus](EventBusDep)
	depRunner   = resource.Require[*Manager](RunnerDep)
)

// observerFactory builds the subagent observer.
type observerFactory struct{}

// Spec implements resource.Factory.
func (observerFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: ObserverResourceKind,
		Impl: ImplLocal,
		Deps: []resource.Requirement{
			depEventBus.Spec(),
			depRunner.Spec(),
		},
	}
}

// New implements resource.Factory. The observer subscribes in
// [Observer.Wire], which the assembly calls once every resource exists.
func (observerFactory) New(_ context.Context, in resource.Input) (*Observer, error) {
	bus, err := depEventBus.Get(in)
	if err != nil {
		return nil, err
	}
	runner, err := depRunner.Get(in)
	if err != nil {
		return nil, err
	}
	return NewObserver(runner, bus)
}

// Register adds the runner factory and the subagent observer factory to
// r. options configure the runner; the observer has no options.
func Register(r *resource.Registry, options ...FactoryOption) error {
	if err := resource.Register(r, NewFactory(options...)); err != nil {
		return err
	}
	return resource.Register(r, observerFactory{})
}
