package openai

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// catalog publishes the deployment's model line-up. It is what lets a host ask
// what a deployment can do before calling it, and what lets the driver refuse a
// request whose model or output was never declared.
type catalog struct {
	models []model.ModelDescriptor
}

// newCatalog takes ownership of a validated line-up.
func newCatalog(models []model.ModelDescriptor) catalog {
	owned := make([]model.ModelDescriptor, len(models))
	for i, descriptor := range models {
		owned[i] = descriptor.Clone()
	}
	return catalog{models: owned}
}

// Models implements [inference.Catalog]. The slice is a copy: a caller that
// mutates what it received must not corrupt the deployment's declarations.
func (c catalog) Models() []model.ModelDescriptor {
	out := make([]model.ModelDescriptor, len(c.models))
	for i, descriptor := range c.models {
		out[i] = descriptor.Clone()
	}
	return out
}

// Describe implements [inference.Catalog].
func (c catalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, descriptor := range c.models {
		if descriptor.ID == id {
			return descriptor.Clone(), true
		}
	}
	return model.ModelDescriptor{}, false
}

// validateModels checks a deployment line-up before it becomes a catalog.
func validateModels(models []model.ModelDescriptor) error {
	seen := make(map[model.ModelID]struct{}, len(models))
	for i, descriptor := range models {
		if err := descriptor.Validate(); err != nil {
			return fmt.Errorf("openai: model %d: %w", i, err)
		}
		if descriptor.ID.Provider != ProviderID {
			return fmt.Errorf(
				"openai: model %q declares provider %q, want %q",
				descriptor.ID.Name, descriptor.ID.Provider, ProviderID,
			)
		}
		if _, ok := seen[descriptor.ID]; ok {
			return fmt.Errorf("openai: model %q is declared twice", descriptor.ID.Name)
		}
		seen[descriptor.ID] = struct{}{}
	}
	return nil
}

// findModel looks a request's model up in the declared line-up.
func findModel(models []model.ModelDescriptor, id model.ModelID) (model.ModelDescriptor, bool) {
	for _, descriptor := range models {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return model.ModelDescriptor{}, false
}

// checkDeclarations enforces the deployment's line-up, when it published one.
//
// An empty line-up means the deployment did not declare anything, and the driver
// stays permissive: it cannot reject a model it never heard of. That is the
// backwards-compatible default, and the reason declarations are opt-in.
func (d *driver) checkDeclarations(request inference.GenerateRequest) error {
	if len(d.settings.Models) == 0 {
		return nil
	}
	descriptor, ok := findModel(d.settings.Models, request.Model.ID)
	if !ok {
		return inference.NewError(
			inference.UnknownModel,
			inference.OperationGenerate,
			inference.FieldModel,
			fmt.Errorf("openai: model %q is not declared by this deployment", request.Model.ID.Name),
		)
	}
	if err := checkDeclaredOutput(descriptor, request); err != nil {
		return err
	}
	return checkDeclaredInputs(descriptor, request)
}

// checkDeclaredOutput rejects an output modality the model never declared. The two
// declaration axes are independent: an empty list on one of them filters nothing
// on that axis and says nothing about the other, which is why the empty-list test
// lives here rather than in checkDeclarations.
func checkDeclaredOutput(
	descriptor model.ModelDescriptor,
	request inference.GenerateRequest,
) error {
	if len(descriptor.Capabilities.Outputs) == 0 {
		return nil
	}
	want := request.Output.PartKind()
	for _, declared := range descriptor.Capabilities.Outputs {
		if declared == want {
			return nil
		}
	}
	return inference.NewError(
		inference.UnsupportedOperation,
		inference.OperationGenerate,
		inference.FieldOutput,
		fmt.Errorf("openai: model %q does not declare %q output", descriptor.ID.Name, want),
	)
}

// checkDeclaredInputs rejects content kinds the model never declared it accepts.
//
// An empty input list declares nothing and filters nothing, matching the output
// side: a deployment that lists no inputs has not stated a restriction.
func checkDeclaredInputs(
	descriptor model.ModelDescriptor,
	request inference.GenerateRequest,
) error {
	if len(descriptor.Capabilities.Inputs) == 0 {
		return nil
	}
	allowed := make(map[message.PartKind]struct{}, len(descriptor.Capabilities.Inputs))
	for _, kind := range descriptor.Capabilities.Inputs {
		allowed[kind] = struct{}{}
	}
	check := func(where string, content message.Content) error {
		for _, part := range content.Parts {
			if _, ok := allowed[part.Kind()]; ok {
				continue
			}
			return inference.NewError(
				inference.UnsupportedFeature,
				inference.OperationGenerate,
				inference.FieldContent,
				fmt.Errorf("openai: model %q does not declare %q input (%s)",
					descriptor.ID.Name, part.Kind(), where),
			)
		}
		return nil
	}
	for i, item := range request.Context {
		if err := check(fmt.Sprintf("context message %d", i), item.Content); err != nil {
			return err
		}
	}
	return check("input", request.Input.Content)
}
