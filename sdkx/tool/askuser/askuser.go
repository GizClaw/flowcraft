package askuser

import (
	"github.com/GizClaw/flowcraft/sdk/tool"
	sdkbuiltin "github.com/GizClaw/flowcraft/sdk/tool/builtin/askuser"
)

// Name is the canonical tool id callers register and LLMs invoke.
// Stable across versions so prompts referring to the tool by name
// keep working. Re-exported from sdk/tool/builtin/askuser during
// the v0.2.0 → v0.5.0 transition; the constant value is identical
// across both paths.
const Name = sdkbuiltin.Name

// New constructs the ask_user tool. The returned value satisfies
// tool.Tool and can be passed to Registry.Register.
//
// During the v0.2.0 → v0.5.0 transition this is a thin forwarder
// to sdk/tool/builtin/askuser.New; sdk cannot import sdkx (the
// dependency runs one-way), so the implementation has to stay in
// sdk for now. At v0.5.0 the implementation moves here verbatim
// and the sdk path is removed — call-site signature does not
// change.
func New() tool.Tool {
	return sdkbuiltin.New()
}
