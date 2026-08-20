package inference

import (
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
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

// ReasoningKind declares a model's reasoning control capability. Zero is the
// conservative declaration: a model without a declared reasoning capability
// has no reasoning channel or reasoning controls.
type ReasoningKind string

const (
	ReasoningNone   ReasoningKind = ""
	ReasoningAlways ReasoningKind = "always"
	ReasoningToggle ReasoningKind = "toggle"
)

func (k ReasoningKind) Validate() error {
	switch k {
	case ReasoningNone, ReasoningAlways, ReasoningToggle:
		return nil
	default:
		return fmt.Errorf("unknown reasoning kind %q", k)
	}
}

// ModelCapabilities describes optional feature bits and content kinds a model
// can serve. Zero is the conservative declaration: every feature the struct
// omits is treated as unsupported until a provider declares it.
type ModelCapabilities struct {
	// Inputs lists the canonical content part kinds the model accepts as
	// request input (text, image, audio, video, tool calls, ...). Providers
	// that do not declare inputs leave the capability unknown rather than
	// asserted absent; routing falls back to operation support and preflight
	// remains the final arbiter.
	Inputs []message.PartKind `json:"inputs,omitempty"`
	// Outputs lists the canonical content part kinds the model can produce as
	// output. Only the output modalities (text, image, audio, video) are
	// representable; routing prefers targets whose declared outputs cover the
	// request intent and skips declared-incompatible tiers. Empty outputs are
	// undeclared and do not filter routing.
	Outputs []message.PartKind `json:"outputs,omitempty"`
	// Reasoning declares the model's reasoning control capability: whether it
	// has a reasoning channel and whether reasoning can be switched or its
	// effort adjusted. Empty (ReasoningNone) is the conservative default.
	Reasoning ReasoningKind `json:"reasoning,omitempty"`
	// HostedWebSearch marks provider-side web_search tool support. It is
	// discovery metadata for hosts; the search configuration itself still
	// rides on GenerateRequest.Extensions as a provider GenerateOptions
	// extension.
	HostedWebSearch bool `json:"hosted_web_search,omitempty"`
}

// Clone returns a defensive copy of the capabilities: the returned value
// shares no backing array with the receiver.
func (c ModelCapabilities) Clone() ModelCapabilities {
	c.Inputs = append([]message.PartKind(nil), c.Inputs...)
	c.Outputs = append([]message.PartKind(nil), c.Outputs...)
	return c
}

// WithInputs returns a copy of the capabilities with the given input content
// kinds appended. The result shares no backing array with the receiver, so
// calls compose safely.
func (c ModelCapabilities) WithInputs(kinds ...message.PartKind) ModelCapabilities {
	c.Inputs = append(append([]message.PartKind(nil), c.Inputs...), kinds...)
	return c
}

// WithOutputs returns a copy of the capabilities with the given output
// content kinds appended. The result shares no backing array with the
// receiver, so calls compose safely.
func (c ModelCapabilities) WithOutputs(kinds ...message.PartKind) ModelCapabilities {
	c.Outputs = append(append([]message.PartKind(nil), c.Outputs...), kinds...)
	return c
}

// WithReasoning returns a copy of the capabilities with the reasoning control
// capability set.
func (c ModelCapabilities) WithReasoning(kind ReasoningKind) ModelCapabilities {
	c.Reasoning = kind
	return c
}

// WithHostedWebSearch returns a copy of the capabilities with hosted web
// search marked supported.
func (c ModelCapabilities) WithHostedWebSearch() ModelCapabilities {
	c.HostedWebSearch = true
	return c
}

func (c ModelCapabilities) Validate() error {
	if err := c.Reasoning.Validate(); err != nil {
		return err
	}
	seenInputs := make(map[message.PartKind]struct{}, len(c.Inputs))
	for _, kind := range c.Inputs {
		if err := kind.Validate(); err != nil {
			return fmt.Errorf("input content kind: %w", err)
		}
		if _, ok := seenInputs[kind]; ok {
			return fmt.Errorf("duplicate input content kind %q", kind)
		}
		seenInputs[kind] = struct{}{}
	}
	seenOutputs := make(map[message.PartKind]struct{}, len(c.Outputs))
	for _, kind := range c.Outputs {
		if !isOutputModality(kind) {
			return fmt.Errorf("output content kind %q is not a representable output modality", kind)
		}
		if _, ok := seenOutputs[kind]; ok {
			return fmt.Errorf("duplicate output content kind %q", kind)
		}
		seenOutputs[kind] = struct{}{}
	}
	return nil
}

// isOutputModality reports whether the content kind is a representable output
// modality: the four kinds the generate intent can request.
func isOutputModality(kind message.PartKind) bool {
	switch kind {
	case message.PartText, message.PartImage, message.PartAudio, message.PartVideo:
		return true
	default:
		return false
	}
}

// ModelLimits declares numeric capacity limits of a model. A nil field
// means the limit is undeclared rather than zero: the provider catalog did
// not claim a value, so callers must not assume an upper bound.
type ModelLimits struct {
	// MaxInputTokens caps the tokens a request may feed to the model as
	// input context (prompt plus any prior turns). Nil when the provider
	// catalog does not declare a limit.
	MaxInputTokens *int `json:"max_input_tokens,omitempty"`
}

func (l ModelLimits) Clone() ModelLimits {
	return ModelLimits{
		MaxInputTokens: clonePointer(l.MaxInputTokens),
	}
}

func (l ModelLimits) Validate() error {
	if l.MaxInputTokens != nil && *l.MaxInputTokens <= 0 {
		return fmt.Errorf("max input tokens must be positive")
	}
	return nil
}

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

// Clone returns a defensive copy of the metadata: the returned value
// shares no backing array with the receiver.
func (m Metadata) Clone() Metadata {
	m.Decisions = append([]Decision(nil), m.Decisions...)
	return m
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
