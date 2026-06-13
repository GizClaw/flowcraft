package claw

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/tool"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// Claw is a local single-agent runtime backed by one workspace.
type Claw struct {
	ws       sdkworkspace.Workspace
	cfg      Config
	agent    agent.Agent
	engine   engine.Engine
	resolver llm.LLMResolver
	tools    *tool.Registry
	history  *historyRuntime
	memory   *memoryRuntime

	toolMu             sync.RWMutex
	toolHandlers       map[string]ToolHandler
	defaultToolHandler ToolHandler

	mu     sync.Mutex
	active *roundController
}

// New constructs a Claw from the fixed workspace config layout.
func New(ws sdkworkspace.Workspace) (_ *Claw, err error) {
	c := &Claw{
		ws: ws,
	}
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	ctx := context.Background()
	cfg, err := loadConfig(ctx, ws)
	if err != nil {
		return nil, err
	}
	c.cfg = cfg
	c.resolver = newProviderSafeLLMResolver(c.buildResolver())
	c.tools = c.buildToolRegistry()
	hist, err := c.buildHistory(ctx)
	if err != nil {
		return nil, err
	}
	c.history = hist
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
	return c.CloseContext(context.Background())
}

// CloseContext releases resources owned by Claw, using ctx to bound
// close-time drains such as async memory extraction.
func (c *Claw) CloseContext(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active != nil {
		active.interrupt(true)
		if err := active.wait(ctx); err != nil {
			return err
		}
	}
	var errs []error
	if c.memory != nil {
		errs = append(errs, c.memory.close(ctx))
	}
	return errors.Join(errs...)
}
