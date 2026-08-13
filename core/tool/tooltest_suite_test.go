package tool_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

func TestTooltestToolSuite(t *testing.T) {
	tooltest.ToolSuite(t, func() tool.Tool {
		return funcTool("suite-tool", "ok")
	})
}

func TestTooltestSourceSuite(t *testing.T) {
	tooltest.SourceSuite(t, func() tool.Source {
		return source{tools: []tool.Tool{funcTool("suite-tool", "ok")}}
	})
}

func TestTooltestCatalogSuite(t *testing.T) {
	tooltest.CatalogSuite(t, func() tool.Catalog {
		reg, err := tool.NewRegistry([]tool.Source{
			source{tools: []tool.Tool{funcTool("suite-tool", "ok")}},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reg.Close() })
		return reg
	})
}
