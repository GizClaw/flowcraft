package anthropic

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// catalog publishes the deployment's model line-up.
type catalog struct {
	models []model.ModelDescriptor
}

func newCatalog(models []model.ModelDescriptor) catalog {
	owned := make([]model.ModelDescriptor, len(models))
	for i, descriptor := range models {
		owned[i] = descriptor.Clone()
	}
	return catalog{models: owned}
}

func (c catalog) Models() []model.ModelDescriptor {
	out := make([]model.ModelDescriptor, len(c.models))
	for i, descriptor := range c.models {
		out[i] = descriptor.Clone()
	}
	return out
}

func (c catalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, descriptor := range c.models {
		if descriptor.ID == id {
			return descriptor.Clone(), true
		}
	}
	return model.ModelDescriptor{}, false
}

var _ inference.Catalog = catalog{}
