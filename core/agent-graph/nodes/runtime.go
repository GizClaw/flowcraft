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
//
// Two things follow, and both are deliberate. The set is a compatibility surface:
// it is the union of what the model and the tool kinds read — a stub for one kind
// implements a method it never calls, and NewCatalogView is derivable from
// Assembly — and adding a sixth method later is a breaking change for every host
// that stubs it. A kind that needs more states its own smaller interface beside
// this one, the way script.Runtime does, rather than widening this one.
type Runtime interface {
	NewCatalogView() engine.CatalogView
	Assembly() *engine.Assembly
	Provider() inference.Provider
	Router() *route.Router
	DefaultModel() model.ModelRef
}
