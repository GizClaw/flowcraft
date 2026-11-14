package bytedance

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

type catalog struct {
	models []model.ModelDescriptor
}

func newCatalog(declared []ModelSpec) catalog {
	models := make([]model.ModelDescriptor, len(declared))
	for i, spec := range declared {
		models[i] = descriptor(spec)
	}
	return catalog{models: models}
}

func (c catalog) Models() []model.ModelDescriptor {
	out := make([]model.ModelDescriptor, len(c.models))
	for i, item := range c.models {
		out[i] = item.Clone()
	}
	return out
}

func (c catalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, item := range c.models {
		if item.ID == id {
			return item.Clone(), true
		}
	}
	return model.ModelDescriptor{}, false
}

var _ inference.Catalog = catalog{}
