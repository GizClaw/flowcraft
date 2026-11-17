package nodes

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/tool/engine"
)

// Runtime is what a node kind needs from the substrate: the model access, the
// catalog view a run projects, and the tool surface a call dispatches through.
//
// It is the whole dependency the built-in kinds have, so the execution layer
// names it instead of the assembly that happens to satisfy it: *runtime.Runtime
// does, and so does a stub of these five methods, which is what lets a kind be
// built and tested without assembling a runtime.
type Runtime interface {
	NewCatalogView() engine.CatalogView
	Assembly() *engine.Assembly
	Provider() inference.Provider
	Router() *route.Router
	DefaultModel() model.ModelRef
}
