package inference

import (
	"fmt"
	"time"
)

type Operation string

const (
	OperationGenerate      Operation = "generate"
	OperationEmbed         Operation = "embed"
	OperationTranscription Operation = "transcription"
	OperationRealtime      Operation = "realtime"
)

func (o Operation) Validate() error {
	switch o {
	case OperationGenerate, OperationEmbed, OperationTranscription, OperationRealtime:
		return nil
	default:
		return fmt.Errorf("unknown inference operation %q", o)
	}
}

// ModelID is the public, credential-free identity of a provider model.
type ModelID struct {
	Provider string `json:"provider" yaml:"provider"`
	Name     string `json:"name" yaml:"name"`
}

func (id ModelID) Validate() error {
	if id.Provider == "" {
		return fmt.Errorf("model provider is required")
	}
	if id.Name == "" {
		return fmt.Errorf("model name is required")
	}
	return nil
}

// ModelRef combines a public model identity with an internal credential
// profile used only while resolving a call.
type ModelRef struct {
	ID      ModelID `json:"id" yaml:"id"`
	Profile string  `json:"profile,omitempty" yaml:"profile,omitempty"`
}

func (r ModelRef) Validate() error {
	return r.ID.Validate()
}

type ModelStatus string

const (
	ModelStatusActive     ModelStatus = "active"
	ModelStatusDeprecated ModelStatus = "deprecated"
	ModelStatusRetired    ModelStatus = "retired"
)

// ModelLifecycle is discovery metadata, not an execution capability claim.
// An empty value means active.
type ModelLifecycle struct {
	Status      ModelStatus `json:"status,omitempty"`
	RetiresAt   *time.Time  `json:"retires_at,omitempty"`
	Replacement *ModelID    `json:"replacement,omitempty"`
	Notes       string      `json:"notes,omitempty"`
}

func (l ModelLifecycle) Clone() ModelLifecycle {
	clone := l
	clone.RetiresAt = clonePointer(l.RetiresAt)
	clone.Replacement = clonePointer(l.Replacement)
	return clone
}

func (l ModelLifecycle) ValidateFor(model ModelID) error {
	status := l.Status
	if status == "" {
		status = ModelStatusActive
	}
	switch status {
	case ModelStatusActive:
		if l.RetiresAt != nil || l.Replacement != nil || l.Notes != "" {
			return fmt.Errorf("active model cannot carry retirement metadata")
		}
	case ModelStatusDeprecated, ModelStatusRetired:
	default:
		return fmt.Errorf("unknown model status %q", l.Status)
	}
	if l.RetiresAt != nil && l.RetiresAt.IsZero() {
		return fmt.Errorf("model retirement time must not be zero")
	}
	if l.Replacement != nil {
		if err := l.Replacement.Validate(); err != nil {
			return fmt.Errorf("replacement: %w", err)
		}
		if *l.Replacement == model {
			return fmt.Errorf("replacement must differ from the model")
		}
	}
	return nil
}

// ModelCapabilities describes optional feature bits a model can serve.
// Zero is the conservative declaration: every feature the struct omits is
// treated as unsupported until a provider declares it.
type ModelCapabilities struct {
	// HostedWebSearch marks provider-side web_search tool support. It is
	// discovery metadata for hosts; the search configuration itself still
	// rides on GenerateRequest.Extensions as a provider GenerateOptions
	// extension.
	HostedWebSearch bool `json:"hosted_web_search,omitempty"`
}

// ModelDescriptor is public discovery metadata. Operations must be derived
// from the drivers registered for the model rather than maintained as a
// separate capability declaration.
type ModelDescriptor struct {
	ID           ModelID           `json:"id"`
	Label        string            `json:"label,omitempty"`
	Operations   []Operation       `json:"operations"`
	Capabilities ModelCapabilities `json:"capabilities,omitzero"`
	Lifecycle    ModelLifecycle    `json:"lifecycle,omitzero"`
}

func (d ModelDescriptor) Clone() ModelDescriptor {
	d.Operations = append([]Operation(nil), d.Operations...)
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
	return d.Lifecycle.ValidateFor(d.ID)
}

// Metadata is attached to every successful operation.
type Metadata struct {
	Model     ModelID    `json:"model"`
	Operation Operation  `json:"operation"`
	Decisions []Decision `json:"decisions,omitempty"`

	// RequestID is the provider-assigned request identifier when the
	// wire response carries one (e.g. DashScope request_id, or an error
	// envelope's request id). Empty when the provider does not expose
	// one. Runtime telemetry mirrors it onto spans as llm.request.id.
	RequestID string `json:"request_id,omitempty"`
	// ResponseID is the provider-assigned identifier of the response
	// object when the wire response carries one (e.g. OpenAI
	// response.id, Anthropic message.id, chat completion id). Empty
	// when unavailable. Runtime telemetry mirrors it onto spans as
	// llm.response.id.
	ResponseID string `json:"response_id,omitempty"`
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
