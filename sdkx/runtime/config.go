// Package runtime assembles and owns a deployment, its runtime integrations,
// event routing, and transport-neutral sessions.
package runtime

import (
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

const (
	defaultIdleTimeout = 10 * time.Minute
	defaultSinkBuffer  = 256
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
	IdleTimeout time.Duration
	SinkBuffer  int
}

// IntegrationConfig configures one independently prepared integration.
type IntegrationConfig struct {
	Name     string
	Kind     string
	Deps     map[string]string
	Settings *deploy.Opaque
}

type configWire struct {
	EventBus  string                  `yaml:"event_bus"`
	Scheduler string                  `yaml:"scheduler,omitempty"`
	Sessions  sessionConfigWire       `yaml:"sessions,omitempty"`
	Items     []integrationConfigWire `yaml:"integrations,omitempty"`
}

type sessionConfigWire struct {
	IdleTimeout *string `yaml:"idle_timeout,omitempty"`
	SinkBuffer  *int    `yaml:"sink_buffer,omitempty"`
}

type integrationConfigWire struct {
	Name     string            `yaml:"name"`
	Kind     string            `yaml:"kind"`
	Deps     map[string]string `yaml:"deps,omitempty"`
	Settings *deploy.Opaque    `yaml:"settings,omitempty"`
}

// DecodeConfig strictly decodes and validates the runtime subtree.
func DecodeConfig(doc deploy.Document) (Config, error) {
	if doc.Runtime == nil {
		return Config{}, errdefs.Validationf("runtime config: runtime section is required")
	}
	wire, err := deploy.DecodeSettings[configWire](doc.Runtime.Node())
	if err != nil {
		return Config{}, errdefs.Validation(fmt.Errorf("runtime config: decode: %w", err))
	}
	cfg := Config{
		EventBus:  strings.TrimSpace(wire.EventBus),
		Scheduler: strings.TrimSpace(wire.Scheduler),
		Sessions: SessionConfig{
			IdleTimeout: defaultIdleTimeout,
			SinkBuffer:  defaultSinkBuffer,
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
