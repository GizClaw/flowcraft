// Package simtools contributes the demo's application-owned simulated tools.
//
// The tool implementations are Go values by design: the native deployment
// document declares policy (which built-in sets are wired, which tools the model
// may call), and a host adds its own tools as a [tool.Source]. There is no
// factory, no resource registry and no deployment name to keep in sync — the
// source is handed to the runtime the host builds.
package simtools

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Source is the simulated tool set. It counts executions, which is what the
// scripted tests assert on: a turn that claims to have played music must have
// actually called the tool.
type Source struct {
	calls atomic.Int64
	tools []tool.Tool
}

// New builds the simulated tool source.
func New() *Source {
	s := &Source{}
	s.tools = []tool.Tool{
		&simulatedTool{
			count: &s.calls,
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
			count: &s.calls,
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
			count: &s.calls,
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
	}
	return s
}

// Tools implements [tool.Source]. Every simulated tool is eager: none needs I/O
// to become available.
func (s *Source) Tools() []tool.Tool { return s.tools }

// LazyTools implements [tool.Source]. The set has nothing to defer.
func (s *Source) LazyTools() []tool.LazyTool { return nil }

// Names lists the tool names in declaration order.
func (s *Source) Names() []string {
	names := make([]string, 0, len(s.tools))
	for _, item := range s.tools {
		names = append(names, item.Definition().Name)
	}
	return names
}

// Calls is how many simulated tools have executed in this process.
func (s *Source) Calls() int64 { return s.calls.Load() }

var _ tool.Source = (*Source)(nil)

type simulatedTool struct {
	definition message.ToolDefinition
	count      *atomic.Int64
}

func (t *simulatedTool) Definition() message.ToolDefinition { return t.definition }

func (t *simulatedTool) Execute(_ context.Context, arguments string) (message.Content, error) {
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
		return message.Content{}, err
	}
	return message.NewTextContent(string(raw)), nil
}
