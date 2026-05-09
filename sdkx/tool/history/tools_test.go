package history_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/tool"
	historytool "github.com/GizClaw/flowcraft/sdkx/tool/history"
)

func TestRegisterTools_RegistersBothNames(t *testing.T) {
	reg := tool.NewRegistry()
	historytool.RegisterTools(reg, historytool.ToolDeps{})

	for _, name := range []string{"history_expand", "history_compact"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("registry missing tool %q after RegisterTools", name)
		}
	}
}
