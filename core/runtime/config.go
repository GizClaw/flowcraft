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
	Sessions SessionConfig
	// DynamicCatalog governs the tool assembly dynamically registered
	// agents run with.
	DynamicCatalog *DynamicCatalogConfig
	// Agents configures the runtime's handling of dynamically registered
	// agents beyond the catalog.
	Agents AgentConfig
	// ExternalDeps declares caller-owned dependency values that the
	// application injects through Builder.WithExternalResource.
	ExternalDeps []ExternalDependency
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

// AgentConfig configures how the runtime treats dynamically registered
// agents.
type AgentConfig struct {
	// RemoveTimeout is the bound UnregisterAgent puts on the drain of a
	// dynamic agent removal when the call gives no WithRemoveTimeout.
	// Zero means the removal is bounded only by the caller's context.
	RemoveTimeout time.Duration
}

type configWire struct {
	EventBus        string                    `json:"event_bus"`
	CheckpointStore string                    `json:"checkpoint_store,omitempty"`
	Sessions        sessionConfigWire         `json:"sessions,omitempty"`
	DynamicCatalog  *dynamicCatalogConfigWire `json:"dynamic_catalog,omitempty"`
	Agents          agentConfigWire           `json:"agents,omitempty"`
	ExternalDeps    []externalDependencyWire  `json:"external_deps,omitempty"`
}

type externalDependencyWire struct {
	Name     string `json:"name"`
	Contract string `json:"contract"`
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

type agentConfigWire struct {
	RemoveTimeout *string `json:"remove_timeout,omitempty"`
}

// DecodeConfig strictly decodes and validates the runtime subtree.
//
// Decoding is one pass per section — sessions, agents, dynamic catalog,
// external dependencies — and a final cross-section check that no
// dependency name collides with a reserved runtime slot. Each section
// decoder owns its defaults and its bounds, so a new field lands in one
// place with its validation next to it.
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
	}
	if cfg.EventBus == "" {
		return Config{}, errdefs.Validationf("runtime config: event_bus is required")
	}
	sessions, err := decodeSessionConfig(wire.Sessions)
	if err != nil {
		return Config{}, err
	}
	cfg.Sessions = sessions
	if cfg.Sessions.Resume && cfg.CheckpointStore == "" {
		return Config{}, errdefs.Validationf(
			"runtime config: sessions.resume requires checkpoint_store")
	}
	agents, err := decodeAgentConfig(wire.Agents)
	if err != nil {
		return Config{}, err
	}
	cfg.Agents = agents
	catalog, err := decodeDynamicCatalog(wire.DynamicCatalog)
	if err != nil {
		return Config{}, err
	}
	cfg.DynamicCatalog = catalog
	externalDeps, err := decodeExternalDeps(wire.ExternalDeps)
	if err != nil {
		return Config{}, err
	}
	cfg.ExternalDeps = externalDeps
	if err := validateExternalConflicts(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// decodeSessionConfig applies the session defaults and decodes every
// declared override. Each bound is checked where the value is read, so a
// rejected document names the field instead of the clamp that failed
// later.
func decodeSessionConfig(wire sessionConfigWire) (SessionConfig, error) {
	cfg := SessionConfig{
		IdleTimeout:             defaultIdleTimeout,
		SinkBuffer:              defaultSinkBuffer,
		SpeculativeBufferEvents: defaultSpeculativeBufferEvents,
		SpeculativeBufferBytes:  defaultSpeculativeBufferBytes,
		DeliveryConcurrency:     defaultDeliveryConcurrency,
		MaxSessions:             defaultMaxSessions,
		Resume:                  false,
	}
	if wire.IdleTimeout != nil {
		timeout, err := positiveDuration("sessions.idle_timeout", *wire.IdleTimeout)
		if err != nil {
			return SessionConfig{}, err
		}
		cfg.IdleTimeout = timeout
	}
	bounded := []struct {
		field string
		value *resource.Int
		limit int
		into  *int
		// positive drops the upper bound: max_sessions is bounded only by
		// what the host can serve, while the buffers have a hard ceiling.
		positive bool
	}{
		{"sessions.sink_buffer", wire.SinkBuffer, maxSinkBuffer, &cfg.SinkBuffer, false},
		{
			"sessions.speculative_buffer_events", wire.SpeculativeBufferEvents,
			maxSpeculativeBufferEvents, &cfg.SpeculativeBufferEvents, false,
		},
		{
			"sessions.speculative_buffer_bytes", wire.SpeculativeBufferBytes,
			maxSpeculativeBufferBytes, &cfg.SpeculativeBufferBytes, false,
		},
		{
			"sessions.delivery_concurrency", wire.DeliveryConcurrency,
			maxDeliveryConcurrency, &cfg.DeliveryConcurrency, false,
		},
		{"sessions.max_sessions", wire.MaxSessions, 0, &cfg.MaxSessions, true},
	}
	for _, field := range bounded {
		if field.value == nil {
			continue
		}
		value := int(*field.value)
		if field.positive {
			if value <= 0 {
				return SessionConfig{}, errdefs.Validationf(
					"runtime config: %s must be positive", field.field)
			}
		} else if value <= 0 || value > field.limit {
			return SessionConfig{}, errdefs.Validationf(
				"runtime config: %s must be between 1 and %d",
				field.field, field.limit)
		}
		*field.into = value
	}
	if wire.Resume != nil {
		cfg.Resume = bool(*wire.Resume)
	}
	return cfg, nil
}

// decodeAgentConfig decodes the runtime's agent-wide settings.
func decodeAgentConfig(wire agentConfigWire) (AgentConfig, error) {
	var cfg AgentConfig
	if wire.RemoveTimeout != nil {
		timeout, err := positiveDuration("agents.remove_timeout", *wire.RemoveTimeout)
		if err != nil {
			return AgentConfig{}, err
		}
		cfg.RemoveTimeout = timeout
	}
	return cfg, nil
}

// decodeDynamicCatalog decodes the dynamic tool catalog: per-agent tool
// resource names, with neither side empty.
func decodeDynamicCatalog(wire *dynamicCatalogConfigWire) (*DynamicCatalogConfig, error) {
	if wire == nil {
		return nil, nil
	}
	cfg := &DynamicCatalogConfig{Tools: wire.Tools}
	if len(cfg.Tools) == 0 {
		return nil, errdefs.Validationf(
			"runtime config: dynamic_catalog.tools must not be empty")
	}
	for agentID, resourceName := range cfg.Tools {
		if strings.TrimSpace(agentID) == "" {
			return nil, errdefs.Validationf(
				"runtime config: dynamic_catalog.tools has an empty agent key")
		}
		if strings.TrimSpace(resourceName) == "" {
			return nil, errdefs.Validationf(
				"runtime config: dynamic_catalog.tools[%q] has an empty tool resource",
				agentID)
		}
	}
	return cfg, nil
}

// decodeExternalDeps decodes the external dependency declarations and
// validates the list as a whole (duplicates, empty names).
func decodeExternalDeps(wire []externalDependencyWire) ([]ExternalDependency, error) {
	deps := make([]ExternalDependency, 0, len(wire))
	for _, dep := range wire {
		deps = append(deps, ExternalDependency{
			Name:     strings.TrimSpace(dep.Name),
			Contract: strings.TrimSpace(dep.Contract),
		})
	}
	if _, err := validateExternalDependencyList(deps); err != nil {
		return nil, err
	}
	return deps, nil
}

// validateExternalConflicts rejects a document whose external dependency
// shadows a reserved runtime slot: the bus, the checkpoint store, or a
// dynamic catalog's tool resource.
func validateExternalConflicts(cfg Config) error {
	externalNames := make(map[string]struct{}, len(cfg.ExternalDeps))
	for _, dep := range cfg.ExternalDeps {
		externalNames[dep.Name] = struct{}{}
	}
	if _, reserved := externalNames[cfg.EventBus]; reserved {
		return errdefs.Validationf(
			"runtime config: event_bus %q cannot be an external dependency",
			cfg.EventBus)
	}
	if _, reserved := externalNames[cfg.CheckpointStore]; cfg.CheckpointStore != "" && reserved {
		return errdefs.Validationf(
			"runtime config: checkpoint_store %q cannot be an external dependency",
			cfg.CheckpointStore)
	}
	if cfg.DynamicCatalog == nil {
		return nil
	}
	for agentID, resourceName := range cfg.DynamicCatalog.Tools {
		if _, reserved := externalNames[resourceName]; reserved {
			return errdefs.Validationf(
				"runtime config: dynamic_catalog.tools[%q] resource %q cannot be an external dependency",
				agentID, resourceName)
		}
	}
	return nil
}

// positiveDuration parses a Go duration string and rejects non-positive
// ones, naming the field so the document knows what to fix.
func positiveDuration(field, value string) (time.Duration, error) {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, errdefs.Validationf("runtime config: %s %q: %v", field, value, err)
	}
	if parsed <= 0 {
		return 0, errdefs.Validationf("runtime config: %s %q: must be positive", field, value)
	}
	return parsed, nil
}
