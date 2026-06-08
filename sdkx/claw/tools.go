package claw

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

const (
	playMusicToolName       = "play_music"
	setDeviceVolumeToolName = "set_device_volume"
)

// ToolHandler is a Claw-hosted tool implementation. args is the raw JSON
// argument object emitted by the model for the named tool.
type ToolHandler func(ctx context.Context, name string, args json.RawMessage) (string, error)

// ToolConfig defines a tool schema in Claw config. Execution is supplied at
// runtime through Handle or HandleDefault.
type ToolConfig struct {
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// ToolConfigs accepts both the current object form and the old string-only
// form:
//
//	tools:
//	  - play_music
//	  - name: set_device_volume
//	    description: Set volume
//	    input_schema: {type: object}
type ToolConfigs []ToolConfig

func (t *ToolConfigs) UnmarshalJSON(raw []byte) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	out := make([]ToolConfig, 0, len(items))
	for i, item := range items {
		var name string
		if err := json.Unmarshal(item, &name); err == nil {
			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("agent.tools[%d]: name is required", i)
			}
			out = append(out, ToolConfig{Name: name})
			continue
		}
		var cfg ToolConfig
		if err := json.Unmarshal(item, &cfg); err != nil {
			return fmt.Errorf("agent.tools[%d]: %w", i, err)
		}
		cfg.Name = strings.TrimSpace(cfg.Name)
		if cfg.Name == "" {
			return fmt.Errorf("agent.tools[%d]: name is required", i)
		}
		out = append(out, cfg)
	}
	*t = out
	return nil
}

func (t ToolConfigs) Names() []string {
	if len(t) == 0 {
		return nil
	}
	out := make([]string, 0, len(t))
	seen := make(map[string]struct{}, len(t))
	for _, cfg := range t {
		name := strings.TrimSpace(cfg.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func (t ToolConfig) definition() model.ToolDefinition {
	schema := t.InputSchema
	if len(schema) == 0 {
		schema = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	return model.ToolDefinition{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: schema,
	}
}

// Handle registers a runtime implementation for one configured tool.
func (c *Claw) Handle(name string, fn ToolHandler) {
	if c == nil {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	c.toolMu.Lock()
	defer c.toolMu.Unlock()
	if c.toolHandlers == nil {
		c.toolHandlers = map[string]ToolHandler{}
	}
	if fn == nil {
		delete(c.toolHandlers, name)
		return
	}
	c.toolHandlers[name] = fn
}

// HandleDefault registers the fallback runtime implementation for configured
// tools without a name-specific handler.
func (c *Claw) HandleDefault(fn ToolHandler) {
	if c == nil {
		return
	}
	c.toolMu.Lock()
	defer c.toolMu.Unlock()
	c.defaultToolHandler = fn
}

func (c *Claw) buildToolRegistry() *tool.Registry {
	reg := tool.NewRegistry()
	for _, cfg := range c.cfg.Agent.Tools {
		def := cfg.definition()
		reg.Register(tool.FuncTool(def, func(_ context.Context, _ string) (string, error) {
			return "", fmt.Errorf("tool %q has no runtime handler", def.Name)
		}))
	}
	reg.Use(func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call model.ToolCall) model.ToolResult {
			if c != nil && c.isConfiguredTool(call.Name) {
				return c.dispatchToolCall(ctx, call)
			}
			return next(ctx, call)
		}
	})
	return reg
}

func (c *Claw) isConfiguredTool(name string) bool {
	for _, configured := range c.cfg.Agent.Tools.Names() {
		if configured == name {
			return true
		}
	}
	return false
}

func (c *Claw) dispatchToolCall(ctx context.Context, call model.ToolCall) model.ToolResult {
	handler := c.toolHandler(call.Name)
	if handler == nil {
		raw, _ := json.Marshal(map[string]any{
			"ignored": true,
			"tool":    call.Name,
		})
		return model.ToolResult{ToolCallID: call.ID, Content: string(raw)}
	}
	content, err := handler(ctx, call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		return model.ToolResult{ToolCallID: call.ID, Content: err.Error(), IsError: true}
	}
	return model.ToolResult{ToolCallID: call.ID, Content: content}
}

func (c *Claw) toolHandler(name string) ToolHandler {
	c.toolMu.RLock()
	defer c.toolMu.RUnlock()
	if c.toolHandlers != nil {
		if h := c.toolHandlers[name]; h != nil {
			return h
		}
	}
	return c.defaultToolHandler
}
