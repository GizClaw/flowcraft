package inference

import "github.com/GizClaw/flowcraft/core/inference/model"

// Catalog exposes the model facts an application declared for this provider:
// which models exist, what they accept and emit, and their limits. A driver that
// ships no model line-up leaves the field nil.
//
// Catalog is how declared capabilities reach the runtime. Without it the
// application's model declarations would be data with no reader.
type Catalog interface {
	// Models returns every declared model. The slice is owned by the
	// provider and must not be mutated by the caller.
	Models() []model.ModelDescriptor
	// Describe returns the descriptor for one model. found is false when
	// the model is not declared.
	Describe(id model.ModelID) (descriptor model.ModelDescriptor, found bool)
}
