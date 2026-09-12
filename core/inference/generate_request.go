package inference

import (
	"context"
	"fmt"
	"maps"

	"github.com/GizClaw/flowcraft/core/message"
)

// GenerateExecutionShape identifies the provider execution contract being
// compiled. A provider may support different canonical fields for unary and
// streaming generation.
type GenerateExecutionShape string

const (
	GenerateExecutionUnary  GenerateExecutionShape = "unary"
	GenerateExecutionStream GenerateExecutionShape = "stream"
)

func (s GenerateExecutionShape) Validate() error {
	switch s {
	case GenerateExecutionUnary, GenerateExecutionStream:
		return nil
	default:
		return fmt.Errorf("unknown generate execution shape %q", s)
	}
}

func (s GenerateExecutionShape) Field() FieldID {
	switch s {
	case GenerateExecutionUnary:
		return FieldGenerateExecutionUnary
	case GenerateExecutionStream:
		return FieldGenerateExecutionStream
	default:
		return ""
	}
}

// GenerateCompiler compiles a canonical Generate request for one explicit
// execution shape without provider I/O.
type GenerateCompiler[Wire any] func(
	context.Context,
	ModelRef,
	GenerateRequest,
	GenerateExecutionShape,
) (Compiled[Wire], error)

// InputRole is deliberately narrower than [message.Message].Role: only a user turn or a
// tool continuation may be the one current input to Generate.
type InputRole string

const (
	InputRoleUser InputRole = "user"
	InputRoleTool InputRole = "tool"
)

type GenerateInput struct {
	Role    InputRole    `json:"role"`
	Content InputContent `json:"content"`
}

func (i GenerateInput) Clone() GenerateInput {
	i.Content = i.Content.Clone()
	return i
}

func (i GenerateInput) Validate() error {
	var role message.Role
	switch i.Role {
	case InputRoleUser:
		role = message.RoleUser
	case InputRoleTool:
		role = message.RoleTool
	default:
		return fmt.Errorf("generate input role must be user or tool")
	}
	if err := i.Content.Validate(); err != nil {
		return err
	}
	return (message.Message{Role: role, Content: i.Content.Content}).Validate()
}

// Message converts an executed input into ordinary history. The returned
// message owns a clone of the parts and cannot retain Intent by construction.
func (i GenerateInput) Message() message.Message {
	return message.Message{Role: message.Role(i.Role), Content: i.Content.Content.Clone()}
}

type GenerateRequest struct {
	Context    []message.Message `json:"context,omitempty" ledger:"generate.context.*.role"`
	Input      GenerateInput     `json:"input" ledger:"generate.input.role"`
	Extensions Extensions        `json:"-" ledger:"extension"`

	// ModelHint is an optional per-call model preference consumed by the
	// inference router's generate selection: "provider/name" or a bare
	// model name. The router honors it only when it names a configured
	// target that can serve the request; otherwise selection falls back
	// to the default policy. Providers never interpret the hint — it is
	// routing metadata, not a provider knob.
	ModelHint string `json:"model_hint,omitempty"`

	// RequestMetadata carries opaque caller/host metadata whose keys and
	// values are deployment-defined (for example conversation or turn
	// identifiers). Core treats it as an opaque bag: it never interprets
	// keys, and each provider driver decides whether and how to surface
	// it on the provider request (metadata, client_metadata, or none).
	RequestMetadata map[string]string `json:"request_metadata,omitempty"`
}

func (r GenerateRequest) Clone() GenerateRequest {
	clone := r
	clone.Context = make([]message.Message, len(r.Context))
	for i, message := range r.Context {
		clone.Context[i] = message.Clone()
	}
	clone.Input = r.Input.Clone()
	clone.Extensions = r.Extensions.Clone()
	clone.RequestMetadata = maps.Clone(r.RequestMetadata)
	return clone
}

func (r GenerateRequest) Validate() error {
	for i, msg := range r.Context {
		if err := msg.Validate(); err != nil {
			return fmt.Errorf("context message %d: %w", i, err)
		}
		if message.HasStreamSource(msg.Content) {
			return fmt.Errorf(
				"context message %d: stream media sources are not allowed in context",
				i,
			)
		}
	}
	if err := r.Input.Validate(); err != nil {
		return fmt.Errorf("generate input: %w", err)
	}
	if message.HasStreamSource(r.Input.Content.Content) {
		return fmt.Errorf("generate input: stream media sources are not allowed")
	}
	return r.Extensions.Validate()
}

func (r GenerateRequest) ActiveFields() []FieldID {
	var fields []FieldID
	if len(r.Context) > 0 {
		fields = append(fields, FieldGenerateContextRole)
		fields = appendGenerateContextPartFields(fields, r.Context)
	}
	if r.Input.Role != "" {
		fields = append(fields, FieldGenerateInputRole)
	}
	fields = appendGenerateInputPartFields(fields, r.Input.Content.Parts)
	fields = appendGenerateIntentFields(fields, r.Input.Content.Intent)
	if len(r.RequestMetadata) > 0 {
		fields = append(fields, FieldGenerateRequestMetadata)
	}
	return r.Extensions.AppendActiveFields(fields)
}

// ActiveFieldsFor returns the complete Generate ledger for one execution
// shape, including the shape itself.
func (r GenerateRequest) ActiveFieldsFor(shape GenerateExecutionShape) []FieldID {
	fields := r.ActiveFields()
	if field := shape.Field(); field != "" {
		fields = append(fields, field)
	}
	return fields
}
