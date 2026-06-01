package claw

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/llm"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// Option configures a Claw before its runtime pieces are built.
type Option func(*Claw)

// WithConfig bypasses workspace config discovery.
func WithConfig(cfg Config) Option {
	return func(c *Claw) {
		cfg.applyDefaults()
		c.cfg = cfg
		c.hasConfig = true
	}
}

// WithConfigRoot changes the workspace subtree New reads JSON config from.
// The directory may contain workspace.json, models.json, memory.json, and
// agent.json. Empty keeps the default "config".
func WithConfigRoot(root string) Option {
	return func(c *Claw) {
		c.configRoot = cleanConfigRoot(root)
	}
}

// WithWorkspaceConfig overrides workspace paths after config loading.
func WithWorkspaceConfig(cfg WorkspaceConfig) Option {
	return func(c *Claw) {
		c.overrides.workspace = &cfg
	}
}

// WithModels overrides model definitions after config loading.
func WithModels(cfg ModelsConfig) Option {
	return func(c *Claw) {
		c.overrides.models = &cfg
	}
}

// WithMemoryConfig overrides memory configuration after config loading.
func WithMemoryConfig(cfg MemoryConfig) Option {
	return func(c *Claw) {
		c.overrides.memory = &cfg
	}
}

// WithAgentConfig overrides agent configuration after config loading.
func WithAgentConfig(cfg AgentConfig) Option {
	return func(c *Claw) {
		c.overrides.agent = &cfg
	}
}

// WithChatModel injects an already-built chat model.
func WithChatModel(client llm.LLM) Option {
	return func(c *Claw) {
		c.chat = client
	}
}

// Claw is a local single-agent runtime backed by one workspace.
type Claw struct {
	ws         sdkworkspace.Workspace
	cfg        Config
	configRoot string
	overrides  configOverrides

	hasConfig bool
	agent     agent.Agent
	engine    engine.Engine
	chat      llm.LLM
	resolver  llm.LLMResolver
	memory    *memoryRuntime

	mu     sync.Mutex
	active map[string]struct{}
}

// New constructs a Claw from a workspace and optional runtime overrides.
func New(ws sdkworkspace.Workspace, opts ...Option) (*Claw, error) {
	c := &Claw{
		ws:         ws,
		configRoot: defaultConfigRoot,
		active:     make(map[string]struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	ctx := context.Background()
	if !c.hasConfig {
		cfg, err := loadConfig(ctx, ws, c.configRoot)
		if err != nil {
			return nil, err
		}
		c.cfg = cfg
	}
	c.cfg.applyOverrides(c.overrides)
	c.cfg.applyDefaults()
	c.cfg.ensureAgentGraph()
	c.resolver = c.buildResolver()
	mem, err := c.buildMemory(ctx)
	if err != nil {
		return nil, err
	}
	c.memory = mem
	c.agent = c.buildAgent()
	eng, err := c.buildEngine()
	if err != nil {
		return nil, err
	}
	c.engine = eng
	return c, nil
}

// Config returns the resolved runtime configuration.
func (c *Claw) Config() Config {
	return c.cfg
}

// Close releases resources owned by Claw.
func (c *Claw) Close() error {
	if c == nil {
		return nil
	}
	var errs []error
	if c.memory != nil {
		errs = append(errs, c.memory.close())
	}
	return errors.Join(errs...)
}
