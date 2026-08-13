// Package simtools registers the demo's application-owned simulated
// tools as a deployment tool.Source resource. The tool implementations
// are Go values by design: the native tool system only declares policy
// (scopes, middleware) in YAML.
package simtools

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
)

// SourceImpl is the tool.Source impl name used by forge deploy
// documents.
const SourceImpl = "sim"

// NewSourceFactory returns the deployment factory for the simulated
// tool source. counter is shared across every built source and counts
// tool executions for test statistics.
func NewSourceFactory(counter *atomic.Int64) resource.Factory {
	return sourceFactory{counter: counter}
}

type sourceFactory struct {
	counter *atomic.Int64
}

// Spec implements resource.Factory.
func (sourceFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "tool.Source",
		Impl: SourceImpl,
	}
}

// New implements resource.Factory.
func (f sourceFactory) New(_ context.Context, in resource.Input) (any, error) {
	if _, err := resource.DecodeTyped[struct{}](in.Settings); err != nil {
		return nil, err
	}
	return &source{tools: f.tools()}, nil
}

func (f sourceFactory) tools() []tool.Tool {
	tools := []tool.Tool{
		&simulatedTool{
			count: f.counter,
			definition: message.ToolDefinition{
				Name:        "play_music",
				Description: "Play a requested song, track, or piece of music.",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"title": {"type": "string", "description": "Song or music title to play."},
						"artist": {"type": "string", "description": "Optional artist or composer name."},
						"reason": {"type": "string", "description": "Short reason inferred from the user request."}
					},
					"required": ["title"]
				}`),
			},
		},
		&simulatedTool{
			count: f.counter,
			definition: message.ToolDefinition{
				Name:        "set_device_volume",
				Description: "Set or adjust the current device volume.",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"pct": {"type": "number", "description": "Target volume percentage from 0 to 100."},
						"delta": {"type": "number", "description": "Relative volume change, positive to increase and negative to decrease."},
						"reason": {"type": "string", "description": "Short reason inferred from the user request."}
					}
				}`),
			},
		},
		&simulatedTool{
			count: f.counter,
			definition: message.ToolDefinition{
				Name:        "stop_playback",
				Description: "Stop the current music or audio playback.",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"reason": {"type": "string", "description": "Short reason inferred from the user request."}
					}
				}`),
			},
		},
		&simulatedTool{
			count: f.counter,
			definition: message.ToolDefinition{
				Name:        "werewolf_game_event",
				Description: "Emit a lifecycle event when Werewolf game state changes.",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"event_type": {"type": "string", "description": "setup, night_resolve, game_over, or continue."},
						"phase": {"type": "string", "description": "Current phase after the event."},
						"detail": {"type": "string", "description": "Internal lifecycle detail. Do not use it for public narration."}
					},
					"required": ["event_type", "phase", "detail"]
				}`),
			},
		},
	}
	return tools
}

// source is the immutable tool.Source value contributing the simulated
// tools.
type source struct {
	tools []tool.Tool
}

// Tools implements tool.Source.
func (s *source) Tools() []tool.Tool { return s.tools }

// LazyTools implements tool.Source.
func (s *source) LazyTools() []tool.LazyTool { return nil }

var _ tool.Source = (*source)(nil)

type simulatedTool struct {
	definition message.ToolDefinition
	count      *atomic.Int64
}

func (t *simulatedTool) Definition() message.ToolDefinition {
	return t.definition
}

func (t *simulatedTool) Execute(_ context.Context, arguments string) (string, error) {
	if t.count != nil {
		t.count.Add(1)
	}
	var parsed any
	out := map[string]any{
		"ok":        true,
		"simulated": true,
		"tool":      t.definition.Name,
	}
	if arguments != "" && json.Unmarshal([]byte(arguments), &parsed) == nil {
		out["args"] = parsed
	} else {
		out["args"] = map[string]any{}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
