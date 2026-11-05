package inference

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// InputRole is deliberately narrower than [message.Role]: only a user turn or a
// tool continuation may be the one current input to a generation.
type InputRole string

const (
	InputRoleUser InputRole = "user"
	InputRoleTool InputRole = "tool"
)

// GenerateInput is the turn being answered.
type GenerateInput struct {
	Role    InputRole       `json:"role"`
	Content message.Content `json:"content"`
}

// Clone returns a deep copy.
func (i GenerateInput) Clone() GenerateInput {
	i.Content = i.Content.Clone()
	return i
}

// Validate checks the role and the content.
func (i GenerateInput) Validate() error {
	if _, err := i.messageRole(); err != nil {
		return err
	}
	return i.Content.Validate()
}

// Message folds an executed input into ordinary history. The returned message
// owns a clone of the parts.
func (i GenerateInput) Message() message.Message {
	return message.Message{Role: message.Role(i.Role), Content: i.Content.Clone()}
}

func (i GenerateInput) messageRole() (message.Role, error) {
	switch i.Role {
	case InputRoleUser:
		return message.RoleUser, nil
	case InputRoleTool:
		return message.RoleTool, nil
	default:
		return "", fmt.Errorf("input role must be %q or %q", InputRoleUser, InputRoleTool)
	}
}

// GenerateRequest is one generation call. It carries only what a
// provider-agnostic caller decides per call: the turn to answer, what to
// produce, and which tools the model may use. Everything else a provider needs
// to know — sampling, output format, reasoning, image size, voice — is the
// driver's deployment configuration, so that adding a provider does not widen
// this struct.
//
// The split is deliberate and testable: a field belongs here when its schema is
// provider-neutral (owned by message or this package) AND a caller sets it per
// call. A value whose schema only one provider understands is an [Extension]; a
// value a caller sets once per deployment is driver settings; cross-cutting
// correlation data travels in the context as [RequestMetadata].
type GenerateRequest struct {
	// Model names the provider-qualified model that should answer.
	Model model.ModelRef `json:"model"`
	// Context is the conversation so far, oldest first.
	Context []message.Message `json:"context,omitempty"`
	// Input is the turn being answered.
	Input GenerateInput `json:"input"`
	// Output is what to produce. Empty means [OutputText].
	Output Output `json:"output,omitempty"`
	// Tools is what the model may call and how the choice is constrained. It
	// applies to [OutputText] only; a request for another output that
	// carries tools is rejected rather than silently ignored. The zero value
	// means no tools.
	Tools Toolset `json:"tools,omitzero"`
	// Extensions carries provider-specific options. Each driver applies only
	// its own and reports the rest as inert.
	Extensions Extensions `json:"-"`
	// ModelHint is an optional per-call model preference ("provider/name" or
	// a bare name) addressed to whatever selection layer a host wraps around
	// the provider. Providers never interpret it.
	ModelHint string `json:"model_hint,omitempty"`
}

// Clone returns a deep copy that shares no mutable state.
func (r GenerateRequest) Clone() GenerateRequest {
	clone := r
	if r.Context != nil {
		clone.Context = make([]message.Message, len(r.Context))
		for i, item := range r.Context {
			clone.Context[i] = item.Clone()
		}
	}
	clone.Input = r.Input.Clone()
	clone.Tools = r.Tools.Clone()
	clone.Extensions = r.Extensions.Clone()
	return clone
}

// Validate checks everything that can be checked without provider knowledge:
// the shape of the request and the internal consistency of its fields. Whether
// the chosen model can serve the chosen output is the driver's check, because
// only it holds the declarations.
func (r GenerateRequest) Validate() error {
	if err := r.Model.Validate(); err != nil {
		return inferenceErr(InvalidRequest, FieldModel, err)
	}
	for i, item := range r.Context {
		if err := item.Validate(); err != nil {
			return inferenceErr(
				InvalidRequest, FieldContext,
				fmt.Errorf("context message %d: %w", i, err),
			)
		}
	}
	if err := r.Input.Validate(); err != nil {
		return inferenceErr(InvalidRequest, FieldInput, err)
	}
	if err := r.Output.Validate(); err != nil {
		return inferenceErr(InvalidRequest, FieldOutput, err)
	}
	if r.Output.Kind() != OutputText && !r.Tools.IsZero() {
		return inferenceErr(
			InvalidRequest, FieldTools,
			fmt.Errorf("tools apply to text output only, not %q", r.Output.Kind()),
		)
	}
	if err := r.Tools.Validate(); err != nil {
		return inferenceErr(InvalidRequest, FieldTools, err)
	}
	if err := r.Extensions.Validate(); err != nil {
		return inferenceErr(InvalidExtension, FieldExtensions, err)
	}
	return nil
}

// ActiveFields lists the canonical fields this request actually sets. A driver
// compares it against what it consumed; the difference is a dropped option and
// must be reported as a [Warning] or an error.
func (r GenerateRequest) ActiveFields() []Field {
	var fields []Field
	if len(r.Context) > 0 {
		fields = append(fields, FieldContext)
	}
	if r.Input.Role != "" {
		fields = append(fields, FieldInput, FieldRole)
	}
	if len(r.Input.Content.Parts) > 0 {
		fields = append(fields, FieldContent)
	}
	if r.Output != "" {
		fields = append(fields, FieldOutput)
	}
	if !r.Tools.IsZero() {
		fields = append(fields, FieldTools)
	}
	if r.Tools.Choice() != nil {
		fields = append(fields, FieldToolChoice)
	}
	if len(r.Extensions) > 0 {
		fields = append(fields, FieldExtensions)
		fields = append(fields, r.Extensions.ActiveFields()...)
	}
	return fields
}

// inferenceErr builds a pre-I/O rejection.
func inferenceErr(kind ErrorKind, field Field, cause error) error {
	return NewError(kind, field, cause)
}
