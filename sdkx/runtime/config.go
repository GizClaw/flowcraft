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
)

const (
	defaultIdleTimeout             = 10 * time.Minute
	defaultSinkBuffer              = 256
	defaultSpeculativeBufferEvents = 1024
	defaultSpeculativeBufferBytes  = 1 << 20
)

// Config is the strictly decoded deploy.Document.Runtime subtree.
type Config struct {
	EventBus     string
	Scheduler    string
	Sessions     SessionConfig
	Integrations []IntegrationConfig
}

// SessionConfig configures the runtime-owned session manager.
type SessionConfig struct {
	IdleTimeout             time.Duration
	SinkBuffer              int
	SpeculativeBufferEvents int
	SpeculativeBufferBytes  int
}

// IntegrationConfig configures one independently prepared integration.
type IntegrationConfig struct {
	Name     string
	Kind     string
	Deps     map[string]string
	Settings *sdkconfig.Opaque
}

type configWire struct {
	EventBus  string                  `json:"event_bus"`
	Scheduler string                  `json:"scheduler,omitempty"`
	Sessions  sessionConfigWire       `json:"sessions,omitempty"`
	Items     []integrationConfigWire `json:"integrations,omitempty"`
}

type sessionConfigWire struct {
	IdleTimeout             *string `json:"idle_timeout,omitempty"`
	SinkBuffer              *int    `json:"sink_buffer,omitempty"`
	SpeculativeBufferEvents *int    `json:"speculative_buffer_events,omitempty"`
	SpeculativeBufferBytes  *int    `json:"speculative_buffer_bytes,omitempty"`
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
		EventBus:  strings.TrimSpace(wire.EventBus),
		Scheduler: strings.TrimSpace(wire.Scheduler),
		Sessions: SessionConfig{
			IdleTimeout:             defaultIdleTimeout,
			SinkBuffer:              defaultSinkBuffer,
			SpeculativeBufferEvents: defaultSpeculativeBufferEvents,
			SpeculativeBufferBytes:  defaultSpeculativeBufferBytes,
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
