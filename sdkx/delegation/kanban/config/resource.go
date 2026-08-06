// Package config adapts kanban delegation backends to deployment
// resources.
package config

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	sdkeventconfig "github.com/GizClaw/flowcraft/sdk/event/config"
	"github.com/GizClaw/flowcraft/sdkx/delegation/kanban"
)

const (
	// ResourceKind is the deployment resource kind implemented by
	// asynchronous delegation backends.
	ResourceKind = "delegation.AsyncBackend"

	// EventBusDep is the optional shared event bus dependency.
	EventBusDep = "event_bus"
)

// MemorySettings is the settings subtree for an in-memory kanban
// backend.
type MemorySettings struct {
	// ScopeID identifies this backend in emitted events. Omission keeps
	// the backend's empty default; an explicitly empty value is invalid.
	ScopeID *string `json:"scope_id,omitempty"`
	// MaxPending caps work waiting to be claimed. Zero means unlimited.
	MaxPending *int `json:"max_pending,omitempty"`
	// MaxCards caps retained cards by evicting terminal cards. Zero
	// means unlimited.
	MaxCards *int `json:"max_cards,omitempty"`
	// CardTTL is a Go duration string. Zero disables age-based eviction.
	CardTTL *string `json:"card_ttl,omitempty"`
}

type memoryDeployFactory struct {
	options []kanban.Option
}

// NewMemoryDeployFactory returns a deployment factory for in-memory
// kanban delegation backends.
//
// Options inject application-owned behavior that the document cannot
// represent, such as a validator. Declarative settings and the optional
// event_bus dependency are applied after these options.
func NewMemoryDeployFactory(options ...kanban.Option) sdkconfig.ResourceFactory {
	return memoryDeployFactory{
		options: slices.Clone(options),
	}
}

func (memoryDeployFactory) Spec() sdkconfig.ResourceSpec {
	return sdkconfig.ResourceSpec{
		Kind: ResourceKind,
		Impl: "kanban-memory",
		Deps: []sdkconfig.ResourceDepSpec{{
			Name: EventBusDep,
			Type: sdkeventconfig.ResourceKind,
		}},
	}
}

func (f memoryDeployFactory) New(_ context.Context, in sdkconfig.Input) (any, error) {
	var settings MemorySettings
	if err := in.Settings.Decode(&settings); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"delegation kanban config: decode memory resource settings: %w", err))
	}

	scopeID := ""
	options := slices.Clone(f.options)
	if settings.ScopeID != nil {
		scopeID = strings.TrimSpace(*settings.ScopeID)
		if scopeID == "" {
			return nil, errdefs.Validationf(
				"delegation kanban config: resource setting scope_id must not be empty")
		}
	}
	if settings.MaxPending != nil {
		if *settings.MaxPending < 0 {
			return nil, errdefs.Validationf(
				"delegation kanban config: resource setting max_pending must not be negative")
		}
		options = append(options, kanban.WithMaxPending(*settings.MaxPending))
	}
	if settings.MaxCards != nil {
		if *settings.MaxCards < 0 {
			return nil, errdefs.Validationf(
				"delegation kanban config: resource setting max_cards must not be negative")
		}
		options = append(options, kanban.WithMaxCards(*settings.MaxCards))
	}
	if settings.CardTTL != nil {
		cardTTL, err := time.ParseDuration(*settings.CardTTL)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"delegation kanban config: resource setting card_ttl: %w", err))
		}
		if cardTTL < 0 {
			return nil, errdefs.Validationf(
				"delegation kanban config: resource setting card_ttl must not be negative")
		}
		options = append(options, kanban.WithCardTTL(cardTTL))
	}

	if value, ok := in.Dep(EventBusDep); ok {
		bus, ok := value.(event.Bus)
		if !ok || isNilBus(bus) {
			return nil, errdefs.Validationf(
				"delegation kanban config: dep %q is %T, want event.Bus",
				EventBusDep, value)
		}
		options = append(options, kanban.WithBus(bus))
	}
	return kanban.New(scopeID, options...), nil
}

func isNilBus(bus event.Bus) bool {
	if bus == nil {
		return true
	}
	value := reflect.ValueOf(bus)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

var _ sdkconfig.ResourceFactory = memoryDeployFactory{}
