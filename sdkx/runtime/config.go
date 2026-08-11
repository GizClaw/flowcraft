// Package runtime assembles and owns a deployment, its runtime integrations,
// event routing, and transport-neutral sessions.
package runtime

import (
	"fmt"
	"strings"
	"time"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/tool/dynamic"
)

const (
	defaultIdleTimeout             = 10 * time.Minute
	defaultSinkBuffer              = 256
	defaultSpeculativeBufferEvents = 1024
	defaultSpeculativeBufferBytes  = 1 << 20
)

// Config is the strictly decoded deploy.Document.Runtime subtree.
type Config struct {
	// EventBus names the deployment resource providing event.Bus.
	EventBus string

	// Scheduler names the deployment resource providing
	// scheduler.Server; empty disables the scheduler.
	Scheduler string

	// CheckpointStore names the deployment resource providing
	// agent.CheckpointStore; empty keeps checkpoints as a host no-op.
	CheckpointStore string

	Sessions     SessionConfig
	Integrations []IntegrationConfig
}

// SessionConfig configures the runtime-owned session manager.
type SessionConfig struct {
	IdleTimeout             time.Duration
	SinkBuffer              int
	SpeculativeBufferEvents int
	SpeculativeBufferBytes  int
	Resume                  bool
	// DynamicCatalog enables the per-session dynamic injection catalog
	// for the configured agents. Nil keeps the current behaviour: the
	// session manager runs without a catalog provider.
	DynamicCatalog *DynamicCatalogConfig
}

// DynamicCatalogConfig is the declarative policy for the per-session
// dynamic catalog. Tools maps agent IDs to tool.Assembly resource
// names; the reserved "default" key is the fallback for agents without
// an explicit entry.
type DynamicCatalogConfig struct {
	Tools             map[string]string
	DefaultExposure   dynamic.Exposure
	Exposures         map[string]dynamic.Exposure
	SelectedRetention int
	RecentWindow      int
	Budget            DynamicBudgetConfig
}

// DynamicBudgetConfig caps how many definitions reach the model per
// turn. Zero fields fall back to the dynamic package defaults.
type DynamicBudgetConfig struct {
	MaxDefinitions int
	MaxBytes       int64
}

// IntegrationConfig configures one independently prepared integration.
type IntegrationConfig struct {
	Name     string
	Kind     string
	Deps     map[string]string
	Settings *sdkconfig.Opaque
}

type configWire struct {
	EventBus        string                  `json:"event_bus"`
	Scheduler       string                  `json:"scheduler,omitempty"`
	CheckpointStore string                  `json:"checkpoint_store,omitempty"`
	Sessions        sessionConfigWire       `json:"sessions,omitempty"`
	Items           []integrationConfigWire `json:"integrations,omitempty"`
}

type sessionConfigWire struct {
	IdleTimeout             *string                   `json:"idle_timeout,omitempty"`
	SinkBuffer              *int                      `json:"sink_buffer,omitempty"`
	SpeculativeBufferEvents *int                      `json:"speculative_buffer_events,omitempty"`
	SpeculativeBufferBytes  *int                      `json:"speculative_buffer_bytes,omitempty"`
	Resume                  *bool                     `json:"resume,omitempty"`
	DynamicCatalog          *dynamicCatalogConfigWire `json:"dynamic_catalog,omitempty"`
}

type dynamicCatalogConfigWire struct {
	Tools             map[string]string           `json:"tools,omitempty"`
	DefaultExposure   *string                     `json:"default_exposure,omitempty"`
	Exposures         map[string]dynamic.Exposure `json:"exposures,omitempty"`
	SelectedRetention *int                        `json:"selected_retention,omitempty"`
	RecentWindow      *int                        `json:"recent_window,omitempty"`
	Budget            *dynamicBudgetConfigWire    `json:"budget,omitempty"`
}

type dynamicBudgetConfigWire struct {
	MaxDefinitions *int   `json:"max_definitions,omitempty"`
	MaxBytes       *int64 `json:"max_bytes,omitempty"`
}

type integrationConfigWire struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Deps     map[string]string `json:"deps,omitempty"`
	Settings *sdkconfig.Opaque `json:"settings,omitempty"`
}

// DecodeConfig strictly decodes and validates the runtime subtree.
func DecodeConfig(doc deploy.Document) (Config, error) {
	if doc.Runtime == nil {
		return Config{}, errdefs.Validationf("runtime config: runtime section is required")
	}
	var wire configWire
	if err := doc.Runtime.Decode(&wire); err != nil {
		return Config{}, errdefs.Validation(fmt.Errorf("runtime config: decode: %w", err))
	}
	cfg := Config{
		EventBus:        strings.TrimSpace(wire.EventBus),
		Scheduler:       strings.TrimSpace(wire.Scheduler),
		CheckpointStore: strings.TrimSpace(wire.CheckpointStore),
		Sessions: SessionConfig{
			IdleTimeout:             defaultIdleTimeout,
			SinkBuffer:              defaultSinkBuffer,
			SpeculativeBufferEvents: defaultSpeculativeBufferEvents,
			SpeculativeBufferBytes:  defaultSpeculativeBufferBytes,
			Resume:                  false,
		},
		Integrations: make([]IntegrationConfig, len(wire.Items)),
	}
	if cfg.EventBus == "" {
		return Config{}, errdefs.Validationf("runtime config: event_bus is required")
	}
	if wire.Sessions.IdleTimeout != nil {
		timeout, parseErr := time.ParseDuration(*wire.Sessions.IdleTimeout)
		if parseErr != nil || timeout <= 0 {
			if parseErr == nil {
				parseErr = fmt.Errorf("must be positive")
			}
			return Config{}, errdefs.Validation(fmt.Errorf(
				"runtime config: sessions.idle_timeout %q: %w",
				*wire.Sessions.IdleTimeout, parseErr))
		}
		cfg.Sessions.IdleTimeout = timeout
	}
	if wire.Sessions.SinkBuffer != nil {
		if *wire.Sessions.SinkBuffer <= 0 {
			return Config{}, errdefs.Validationf(
				"runtime config: sessions.sink_buffer must be positive")
		}
		cfg.Sessions.SinkBuffer = *wire.Sessions.SinkBuffer
	}
	if wire.Sessions.SpeculativeBufferEvents != nil {
		if *wire.Sessions.SpeculativeBufferEvents <= 0 {
			return Config{}, errdefs.Validationf(
				"runtime config: sessions.speculative_buffer_events must be positive")
		}
		cfg.Sessions.SpeculativeBufferEvents = *wire.Sessions.SpeculativeBufferEvents
	}
	if wire.Sessions.SpeculativeBufferBytes != nil {
		if *wire.Sessions.SpeculativeBufferBytes <= 0 {
			return Config{}, errdefs.Validationf(
				"runtime config: sessions.speculative_buffer_bytes must be positive")
		}
		cfg.Sessions.SpeculativeBufferBytes = *wire.Sessions.SpeculativeBufferBytes
	}
	if wire.Sessions.Resume != nil {
		cfg.Sessions.Resume = *wire.Sessions.Resume
	}
	if cfg.Sessions.Resume && cfg.CheckpointStore == "" {
		return Config{}, errdefs.Validationf(
			"runtime config: sessions.resume requires checkpoint_store")
	}
	if wire.Sessions.DynamicCatalog != nil {
		catalog, err := decodeDynamicCatalog(wire.Sessions.DynamicCatalog)
		if err != nil {
			return Config{}, err
		}
		cfg.Sessions.DynamicCatalog = catalog
	}

	seenNames := make(map[string]struct{}, len(wire.Items))
	for i, item := range wire.Items {
		name := strings.TrimSpace(item.Name)
		kind := strings.TrimSpace(item.Kind)
		where := fmt.Sprintf("runtime config: integrations[%d]", i)
		if name == "" {
			return Config{}, errdefs.Validationf("%s.name is required", where)
		}
		if _, exists := seenNames[name]; exists {
			return Config{}, errdefs.Validationf(
				"runtime config: duplicate integration name %q", name)
		}
		seenNames[name] = struct{}{}
		if kind == "" {
			return Config{}, errdefs.Validationf("%s.kind is required", where)
		}
		deps := make(map[string]string, len(item.Deps))
		for depName, ref := range item.Deps {
			depName = strings.TrimSpace(depName)
			ref = strings.TrimSpace(ref)
			if depName == "" {
				return Config{}, errdefs.Validationf("%s.deps has an empty name", where)
			}
			if ref == "" {
				return Config{}, errdefs.Validationf("%s.deps[%q] is empty", where, depName)
			}
			if strings.Contains(ref, deploy.RefSeparator) {
				return Config{}, errdefs.Validationf(
					"%s.deps[%q] reference %q must name a whole resource",
					where, depName, ref)
			}
			deps[depName] = ref
		}
		cfg.Integrations[i] = IntegrationConfig{
			Name: name, Kind: kind, Deps: deps, Settings: item.Settings,
		}
	}
	return cfg, nil
}

// decodeDynamicCatalog strictly decodes and validates the
// sessions.dynamic_catalog subtree. Resource existence and agent
// coverage are deployment facts, so they are checked by the Builder
// rather than here.
func decodeDynamicCatalog(wire *dynamicCatalogConfigWire) (*DynamicCatalogConfig, error) {
	if len(wire.Tools) == 0 {
		return nil, errdefs.Validationf(
			"runtime config: sessions.dynamic_catalog.tools must map agent IDs to tool resource names")
	}
	tools := make(map[string]string, len(wire.Tools))
	for agentID, resource := range wire.Tools {
		if strings.TrimSpace(agentID) == "" {
			return nil, errdefs.Validationf(
				"runtime config: sessions.dynamic_catalog.tools has an empty agent ID")
		}
		if strings.TrimSpace(resource) == "" {
			return nil, errdefs.Validationf(
				"runtime config: sessions.dynamic_catalog.tools[%q] is empty",
				agentID)
		}
		tools[agentID] = resource
	}

	cfg := &DynamicCatalogConfig{
		Tools:             tools,
		DefaultExposure:   dynamic.ExposureDeferred,
		Exposures:         map[string]dynamic.Exposure{},
		SelectedRetention: 0,
		RecentWindow:      0,
	}
	if wire.DefaultExposure != nil {
		exp := dynamic.Exposure(strings.TrimSpace(*wire.DefaultExposure))
		if exp == "" {
			return nil, errdefs.Validationf(
				"runtime config: sessions.dynamic_catalog.default_exposure is empty")
		}
		if !exp.Valid() {
			return nil, errdefs.Validationf(
				"runtime config: sessions.dynamic_catalog.default_exposure %q is invalid",
				exp)
		}
		cfg.DefaultExposure = exp
	}
	for name, exp := range wire.Exposures {
		if strings.TrimSpace(name) == "" {
			return nil, errdefs.Validationf(
				"runtime config: sessions.dynamic_catalog.exposures has an empty tool name")
		}
		if !exp.Valid() {
			return nil, errdefs.Validationf(
				"runtime config: sessions.dynamic_catalog.exposures[%s] %q is invalid",
				name, exp)
		}
		cfg.Exposures[name] = exp
	}
	if wire.SelectedRetention != nil {
		if *wire.SelectedRetention < 0 {
			return nil, errdefs.Validationf(
				"runtime config: sessions.dynamic_catalog.selected_retention must not be negative")
		}
		cfg.SelectedRetention = *wire.SelectedRetention
	}
	if wire.RecentWindow != nil {
		if *wire.RecentWindow < 0 {
			return nil, errdefs.Validationf(
				"runtime config: sessions.dynamic_catalog.recent_window must not be negative")
		}
		cfg.RecentWindow = *wire.RecentWindow
	}
	if wire.Budget != nil {
		if wire.Budget.MaxDefinitions != nil {
			if *wire.Budget.MaxDefinitions < 0 {
				return nil, errdefs.Validationf(
					"runtime config: sessions.dynamic_catalog.budget.max_definitions must not be negative")
			}
			cfg.Budget.MaxDefinitions = *wire.Budget.MaxDefinitions
		}
		if wire.Budget.MaxBytes != nil {
			if *wire.Budget.MaxBytes < 0 {
				return nil, errdefs.Validationf(
					"runtime config: sessions.dynamic_catalog.budget.max_bytes must not be negative")
			}
			cfg.Budget.MaxBytes = *wire.Budget.MaxBytes
		}
	}
	return cfg, nil
}
