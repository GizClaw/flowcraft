// Package config adapts event buses to deployment resources.
package config

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
)

// ResourceKind is the deployment resource kind implemented by event
// buses.
const ResourceKind = "event.Bus"

// MemorySettings is the settings subtree for the memory bus resource.
type MemorySettings struct {
	// RouteCacheSize bounds subject-route caching. Zero disables the
	// cache, a negative value keeps the event package default, and
	// omission also keeps the default.
	RouteCacheSize *int `json:"route_cache_size,omitempty"`
}

type memoryDeployFactory struct {
	options []event.MemoryBusOption
}

// NewMemoryDeployFactory returns a factory for in-process event buses.
//
// Options inject application-owned dependencies that the document
// cannot represent, such as an [event.Observer]. Declarative route-cache
// settings are applied after these options.
func NewMemoryDeployFactory(options ...event.MemoryBusOption) config.ResourceFactory {
	return memoryDeployFactory{
		options: append([]event.MemoryBusOption(nil), options...),
	}
}

func (memoryDeployFactory) Spec() config.ResourceSpec {
	return config.ResourceSpec{Kind: ResourceKind, Impl: "memory"}
}

func (f memoryDeployFactory) New(_ context.Context, in config.Input) (any, error) {
	var settings MemorySettings
	if err := in.Settings.Decode(&settings); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"event config: decode memory resource settings: %w", err))
	}
	options := append([]event.MemoryBusOption(nil), f.options...)
	if settings.RouteCacheSize != nil {
		options = append(options, event.WithRouteCacheSize(*settings.RouteCacheSize))
	}
	return event.NewMemoryBus(options...), nil
}
