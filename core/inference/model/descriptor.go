package model

import "fmt"

// ModelDescriptor is public discovery metadata. Operations must be derived
// from the drivers registered for the model rather than maintained as a
// separate capability declaration.
type ModelDescriptor struct {
	ID           ModelID           `json:"id"`
	Label        string            `json:"label,omitempty"`
	Operations   []Operation       `json:"operations"`
	Capabilities ModelCapabilities `json:"capabilities,omitzero"`
	Limits       ModelLimits       `json:"limits,omitzero"`
	Lifecycle    ModelLifecycle    `json:"lifecycle,omitzero"`
}

func (d ModelDescriptor) Clone() ModelDescriptor {
	d.Operations = append([]Operation(nil), d.Operations...)
	d.Capabilities = d.Capabilities.Clone()
	d.Limits = d.Limits.Clone()
	d.Lifecycle = d.Lifecycle.Clone()
	return d
}

func (d ModelDescriptor) Validate() error {
	if err := d.ID.Validate(); err != nil {
		return err
	}
	seen := make(map[Operation]struct{}, len(d.Operations))
	for _, operation := range d.Operations {
		if err := operation.Validate(); err != nil {
			return err
		}
		if _, ok := seen[operation]; ok {
			return fmt.Errorf("duplicate model operation %q", operation)
		}
		seen[operation] = struct{}{}
	}
	if d.Capabilities.HostedWebSearch {
		if _, ok := seen[OperationGenerate]; !ok {
			return fmt.Errorf(
				"hosted web search requires the generate operation",
			)
		}
	}
	if err := d.Capabilities.Validate(); err != nil {
		return err
	}
	if err := d.Limits.Validate(); err != nil {
		return err
	}
	return d.Lifecycle.ValidateFor(d.ID)
}
