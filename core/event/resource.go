package event

import (
	"context"

	"github.com/GizClaw/flowcraft/core/resource"
)

// Settings is the strict settings subtree of the memory bus factory.
type Settings struct {
	// RouteCacheSize mirrors [WithRouteCacheSize]: a positive value
	// caps the subject route cache, zero disables it, and a negative
	// or omitted value keeps the package default.
	RouteCacheSize *int `json:"route_cache_size,omitempty"`
}

// Factory builds an in-process memory bus as the event.Bus resource.
type Factory struct {
	options []MemoryBusOption
}

// NewFactory returns a memory bus factory. Options inject
// application-owned dependencies the document cannot express (for
// example [WithObserver]); document settings are applied afterwards.
func NewFactory(opts ...MemoryBusOption) Factory {
	return Factory{options: append([]MemoryBusOption(nil), opts...)}
}

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{Kind: "event.Bus", Impl: "memory"}
}

// New implements resource.Factory.
func (f Factory) New(_ context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[Settings](in.Settings)
	if err != nil {
		return nil, err
	}
	options := append([]MemoryBusOption(nil), f.options...)
	if settings.RouteCacheSize != nil {
		options = append(options, WithRouteCacheSize(*settings.RouteCacheSize))
	}
	return NewMemoryBus(options...), nil
}

// Register adds the memory bus factory to the registry.
func Register(r *resource.Registry) error {
	return r.Register(NewFactory())
}
