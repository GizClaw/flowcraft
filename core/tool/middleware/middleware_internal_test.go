package middleware

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

type testSource struct {
	tools []tool.Tool
}

func (s testSource) Tools() []tool.Tool         { return s.tools }
func (s testSource) LazyTools() []tool.LazyTool { return nil }

func catalogWith(tools ...tool.Tool) *tool.Registry {
	reg, err := tool.NewRegistry([]tool.Source{testSource{tools: tools}})
	if err != nil {
		panic(err)
	}
	return reg
}

func echoTool(name string) tool.Tool {
	return tool.FuncTool(message.ToolDefinition{Name: name},
		func(_ context.Context, args string) (string, error) {
			return "echo:" + args, nil
		})
}

func call(name string) message.ToolCall {
	return message.ToolCall{ID: "call-1", Name: name, Arguments: json.RawMessage(`{"x":1}`)}
}
