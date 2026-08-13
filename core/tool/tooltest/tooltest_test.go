package tooltest_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

func TestFixturesAndSuites(t *testing.T) {
	echo := tooltest.FuncTool("echo", "echoes", func(_ context.Context, args string) (string, error) {
		return args, nil
	})
	tooltest.ToolSuite(t, func() tool.Tool { return echo })
	tooltest.SourceSuite(t, func() tool.Source { return tooltest.Source(echo) })
	tooltest.CatalogSuite(t, func() tool.Catalog {
		return tooltest.Catalog(t, echo)
	})
}

func TestRecordingTool(t *testing.T) {
	rec := tooltest.NewRecordingTool("rec")
	rec.SetResponse("ok", nil)
	got, err := rec.Execute(context.Background(), `{"a":1}`)
	if err != nil || got != "ok" {
		t.Fatalf("Execute = (%q, %v)", got, err)
	}
	calls := rec.Calls()
	if len(calls) != 1 || calls[0].Arguments != `{"a":1}` {
		t.Fatalf("calls = %+v", calls)
	}
}
