// Package runtime assembles and owns a deployment, its event routing,
// and transport-neutral sessions.
package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

const (
	defaultIdleTimeout             = 10 * time.Minute
	defaultSinkBuffer              = 256
	defaultSpeculativeBufferEvents = 1024
	defaultSpeculativeBufferBytes  = 1 << 20
	defaultDeliveryConcurrency     = 8
	defaultMaxSessions             = 1024
	maxSinkBuffer                  = 1 << 20
	maxSpeculativeBufferEvents     = 1 << 20
	maxSpeculativeBufferBytes      = 256 << 20
	maxDeliveryConcurrency         = 1024
)

// Config is the strictly decoded deploy.Document.Runtime subtree.
type Config struct {
	// EventBus names the deployment resource providing event.Bus.
	EventBus string
	// CheckpointStore names the deployment resource providing
	// agent.CheckpointStore; empty keeps checkpoints as a host no-op.
	CheckpointStore string
	// Sessions configures the runtime-owned session manager.
	Sessions       SessionConfig
	DynamicCatalog *DynamicCatalogConfig
}

// SessionConfig configures the runtime-owned session manager.
type SessionConfig struct {
	IdleTimeout             time.Duration
	SinkBuffer              int
	SpeculativeBufferEvents int
	SpeculativeBufferBytes  int
	DeliveryConcurrency     int
	MaxSessions             int
	Resume                  bool
}

// DynamicCatalogConfig maps agent IDs to tool.Assembly resource names;
// the reserved "default" key is the fallback for agents without an
// explicit entry. The injection policy itself lives in each
// tool.Assembly's dynamic settings — the runtime only wires the
// assembly and creates per-session views.
type DynamicCatalogConfig struct {
	Tools map[string]string
}

type configWire struct {
	EventBus        string                    `json:"event_bus"`
	CheckpointStore string                    `json:"checkpoint_store,omitempty"`
	Sessions        sessionConfigWire         `json:"sessions,omitempty"`
	DynamicCatalog  *dynamicCatalogConfigWire `json:"dynamic_catalog,omitempty"`
}

type sessionConfigWire struct {
	IdleTimeout             *string        `json:"idle_timeout,omitempty"`
	SinkBuffer              *resource.Int  `json:"sink_buffer,omitempty"`
	SpeculativeBufferEvents *resource.Int  `json:"speculative_buffer_events,omitempty"`
	SpeculativeBufferBytes  *resource.Int  `json:"speculative_buffer_bytes,omitempty"`
	DeliveryConcurrency     *resource.Int  `json:"delivery_concurrency,omitempty"`
	MaxSessions             *resource.Int  `json:"max_sessions,omitempty"`
	Resume                  *resource.Bool `json:"resume,omitempty"`
}

type dynamicCatalogConfigWire struct {
	Tools map[string]string `json:"tools,omitempty"`
}

// DecodeConfig strictly decodes and validates the runtime subtree.
func DecodeConfig(ctx context.Context, doc deploy.Document) (Config, error) {
	if doc.Runtime == nil {
		return Config{}, errdefs.Validationf("runtime config: runtime section is required")
	}
	var wire configWire
	if err := resource.DecodeSettings(ctx, &wire, doc.Runtime.Bytes(), resource.ExpandEnv()); err != nil {
		return Config{}, errdefs.Validation(fmt.Errorf("runtime config: decode: %w", err))
	}
	cfg := Config{
		EventBus:        strings.TrimSpace(wire.EventBus),
		CheckpointStore: strings.TrimSpace(wire.CheckpointStore),
		Sessions: SessionConfig{
			IdleTimeout:             defaultIdleTimeout,
			SinkBuffer:              defaultSinkBuffer,
			SpeculativeBufferEvents: defaultSpeculativeBufferEvents,
			SpeculativeBufferBytes:  defaultSpeculativeBufferBytes,
			DeliveryConcurrency:     defaultDeliveryConcurrency,
			MaxSessions:             defaultMaxSessions,
			Resume:                  false,
		},
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
		if *wire.Sessions.SinkBuffer <= 0 ||
			*wire.Sessions.SinkBuffer > maxSinkBuffer {
			return Config{}, errdefs.Validationf(
				"runtime config: sessions.sink_buffer must be between 1 and %d",
				maxSinkBuffer)
		}
		cfg.Sessions.SinkBuffer = int(*wire.Sessions.SinkBuffer)
	}
	if wire.Sessions.SpeculativeBufferEvents != nil {
		if *wire.Sessions.SpeculativeBufferEvents <= 0 ||
			*wire.Sessions.SpeculativeBufferEvents > maxSpeculativeBufferEvents {
			return Config{}, errdefs.Validationf(
				"runtime config: sessions.speculative_buffer_events must be between 1 and %d",
				maxSpeculativeBufferEvents)
		}
		cfg.Sessions.SpeculativeBufferEvents = int(*wire.Sessions.SpeculativeBufferEvents)
	}
	if wire.Sessions.SpeculativeBufferBytes != nil {
		if *wire.Sessions.SpeculativeBufferBytes <= 0 ||
			*wire.Sessions.SpeculativeBufferBytes > maxSpeculativeBufferBytes {
			return Config{}, errdefs.Validationf(
				"runtime config: sessions.speculative_buffer_bytes must be between 1 and %d",
				maxSpeculativeBufferBytes)
		}
		cfg.Sessions.SpeculativeBufferBytes = int(*wire.Sessions.SpeculativeBufferBytes)
	}
	if wire.Sessions.DeliveryConcurrency != nil {
		if *wire.Sessions.DeliveryConcurrency <= 0 ||
			*wire.Sessions.DeliveryConcurrency > maxDeliveryConcurrency {
			return Config{}, errdefs.Validationf(
				"runtime config: sessions.delivery_concurrency must be between 1 and %d",
				maxDeliveryConcurrency)
		}
		cfg.Sessions.DeliveryConcurrency = int(*wire.Sessions.DeliveryConcurrency)
	}
	if wire.Sessions.MaxSessions != nil {
		if *wire.Sessions.MaxSessions <= 0 {
			return Config{}, errdefs.Validationf(
				"runtime config: sessions.max_sessions must be positive")
		}
		cfg.Sessions.MaxSessions = int(*wire.Sessions.MaxSessions)
	}
	if wire.Sessions.Resume != nil {
		cfg.Sessions.Resume = bool(*wire.Sessions.Resume)
	}
	if cfg.Sessions.Resume && cfg.CheckpointStore == "" {
		return Config{}, errdefs.Validationf(
			"runtime config: sessions.resume requires checkpoint_store")
	}
	if wire.DynamicCatalog != nil {
		cfg.DynamicCatalog = &DynamicCatalogConfig{
			Tools: wire.DynamicCatalog.Tools,
		}
		if len(cfg.DynamicCatalog.Tools) == 0 {
			return Config{}, errdefs.Validationf(
				"runtime config: dynamic_catalog.tools must not be empty")
		}
		for agentID, resourceName := range cfg.DynamicCatalog.Tools {
			if strings.TrimSpace(agentID) == "" {
				return Config{}, errdefs.Validationf(
					"runtime config: dynamic_catalog.tools has an empty agent key")
			}
			if strings.TrimSpace(resourceName) == "" {
				return Config{}, errdefs.Validationf(
					"runtime config: dynamic_catalog.tools[%q] has an empty tool resource",
					agentID)
			}
		}
	}
	return cfg, nil
}
