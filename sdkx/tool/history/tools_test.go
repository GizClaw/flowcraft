package history_test

import (
	"testing"

	sdkhistory "github.com/GizClaw/flowcraft/sdk/history"
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

// TypeAliasInterop verifies the Go type alias contract: a value of
// the sdkx ToolDeps must be assignable to sdkhistory.ToolDeps without
// conversion (and vice-versa). This guards against an accidental
// future switch from `type Foo = sdkhistory.Foo` to `type Foo
// sdkhistory.Foo`, which would silently break user code.
func TestTypeAliasInterop(t *testing.T) {
	var sdkx historytool.ToolDeps
	var sdk sdkhistory.ToolDeps = sdkx
	_ = sdk

	var sdk2 sdkhistory.ToolDeps
	var sdkx2 historytool.ToolDeps = sdk2
	_ = sdkx2
}
