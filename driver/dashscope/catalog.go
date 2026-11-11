package dashscope

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

type catalog struct{ models []model.ModelDescriptor }

func newCatalog(declared []ModelSpec) catalog {
	models := make([]model.ModelDescriptor, len(declared))
	for i, spec := range declared {
		models[i] = model.ModelDescriptor{ID: model.ModelID{Provider: ProviderID, Name: spec.Name}, Capabilities: spec.Capabilities.Clone(), Limits: spec.Limits.Clone(), Lifecycle: spec.Lifecycle.Clone()}
	}
	return catalog{models: models}
}

func (c catalog) Models() []model.ModelDescriptor {
	out := make([]model.ModelDescriptor, len(c.models))
	for i, m := range c.models {
		out[i] = m.Clone()
	}
	return out
}
func (c catalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, m := range c.models {
		if m.ID == id {
			return m.Clone(), true
		}
	}
	return model.ModelDescriptor{}, false
}

var _ inference.Catalog = catalog{}
