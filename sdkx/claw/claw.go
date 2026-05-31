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

// WithChatModel injects an already-built chat model.
func WithChatModel(client llm.LLM) Option {
	return func(c *Claw) {
		c.chat = client
	}
}

// Claw is a local single-agent runtime backed by one workspace.
type Claw struct {
	ws  sdkworkspace.Workspace
	cfg Config

	hasConfig bool
	agent     agent.Agent
	engine    engine.Engine
	chat      llm.LLM
	memory    *memoryRuntime

	mu     sync.Mutex
	active map[string]struct{}
}

// New constructs a Claw from a workspace and optional runtime overrides.
func New(ws sdkworkspace.Workspace, opts ...Option) (*Claw, error) {
	c := &Claw{
		ws:     ws,
		active: make(map[string]struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	ctx := context.Background()
	if !c.hasConfig {
		cfg, err := loadConfig(ctx, ws)
		if err != nil {
			return nil, err
		}
		c.cfg = cfg
	}
	if c.chat == nil {
		client, err := c.chatModel(ctx)
		if err != nil {
			return nil, err
		}
		c.chat = client
	}
	mem, err := c.buildMemory(ctx)
	if err != nil {
		return nil, err
	}
	c.memory = mem
	c.agent = c.buildAgent()
	c.engine = c.buildEngine(c.chat)
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
