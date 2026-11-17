package script

import "github.com/GizClaw/flowcraft/core/tool/engine"

// Runtime is what the script node needs from the substrate: a fresh catalog view
// for a run and the assembly a tool call dispatches through.
//
// It is narrower than the nodes package's interface on purpose — a script host
// never reads the model access — and it is what *runtime.Runtime satisfies
// without this package depending on the assembly.
type Runtime interface {
	NewCatalogView() engine.CatalogView
	Assembly() *engine.Assembly
}
