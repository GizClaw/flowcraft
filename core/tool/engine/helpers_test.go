package engine_test

import (
	"context"

	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// funcTool and source are the shared test doubles the engine tests were written
// against. They live here rather than in the root package's tests because a test
// file cannot span packages: the root keeps its own copy for its own tests.

func funcTool(name, content string) tool.Tool {
	return tooltest.FuncTool(name, "", func(context.Context, string) (string, error) {
		return content, nil
	})
}

type source struct {
	tools     []tool.Tool
	lazyTools []tool.LazyTool
}

func (s source) Tools() []tool.Tool         { return s.tools }
func (s source) LazyTools() []tool.LazyTool { return s.lazyTools }
