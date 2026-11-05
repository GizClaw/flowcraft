package model

// ModelDescriptor is public discovery metadata: who the model is, what content
// it accepts and emits, what it limits, and how long it lives.
//
// There is no operation list. What a model can do is expressed by the kinds it
// declares — a model whose Inputs include audio and whose Outputs are text
// serves transcription, and no separate label says so twice.
type ModelDescriptor struct {
	ID           ModelID           `json:"id"`
	Label        string            `json:"label,omitempty"`
	Capabilities ModelCapabilities `json:"capabilities,omitzero"`
	Limits       ModelLimits       `json:"limits,omitzero"`
	Lifecycle    ModelLifecycle    `json:"lifecycle,omitzero"`
}

func (d ModelDescriptor) Clone() ModelDescriptor {
	d.Capabilities = d.Capabilities.Clone()
	d.Limits = d.Limits.Clone()
	d.Lifecycle = d.Lifecycle.Clone()
	return d
}

func (d ModelDescriptor) Validate() error {
	if err := d.ID.Validate(); err != nil {
		return err
	}
	if err := d.Capabilities.Validate(); err != nil {
		return err
	}
	if err := d.Limits.Validate(); err != nil {
		return err
	}
	return d.Lifecycle.ValidateFor(d.ID)
}
