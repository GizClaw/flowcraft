package scriptrt

import (
	"github.com/GizClaw/flowcraft/core/agent/scriptrt/jsrt"
	"github.com/GizClaw/flowcraft/core/agent/scriptrt/luart"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by script
// runtimes.
const ResourceKind = "agent.ScriptRuntime"

// Register adds the JavaScript and Lua script runtime factories to r.
func Register(r *resource.Registry) error {
	if err := jsrt.Register(r); err != nil {
		return err
	}
	return luart.Register(r)
}
