package compiler

import (
	"maps"

	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/entityfact"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const errPrefix = "memory/internal/compiler"

// SourceKind identifies a canonical source required by a memory assembly.
type SourceKind string

const (
	SourceMessageLog    SourceKind = "message_log"
	SourceDocumentStore SourceKind = "document_store"
)

// Capability identifies a stable semantic memory capability.
type Capability string

const (
	CapabilityRecentWindow    Capability = "recent_window"
	CapabilitySummaryDAG      Capability = "summary_dag"
	CapabilityDocumentChunks  Capability = "document_chunks"
	CapabilityMessageIndex    Capability = "message_index"
	CapabilityEntityFactIndex Capability = "entity_fact_index"
)

// Spec declares the capabilities a memory runtime needs without naming a
// product recipe or mode.
type Spec struct {
	Sources      []SourceSpec        `json:"sources,omitempty" yaml:"sources,omitempty"`
	Capabilities []CapabilitySpec    `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Projections  []ProjectionRequest `json:"projections,omitempty" yaml:"projections,omitempty"`
	WriteStages  []StageSpec         `json:"write_stages,omitempty" yaml:"write_stages,omitempty"`
	ReadStages   []StageSpec         `json:"read_stages,omitempty" yaml:"read_stages,omitempty"`
	Lifecycle    []StageSpec         `json:"lifecycle,omitempty" yaml:"lifecycle,omitempty"`
	Diagnostics  []StageSpec         `json:"diagnostics,omitempty" yaml:"diagnostics,omitempty"`
}

// SourceSpec declares a canonical source dependency.
type SourceSpec struct {
	Kind     SourceKind `json:"kind" yaml:"kind"`
	Required bool       `json:"required,omitempty" yaml:"required,omitempty"`
}

// CapabilitySpec declares one semantic view capability.
type CapabilitySpec struct {
	Capability Capability `json:"capability" yaml:"capability"`
	Required   bool       `json:"required,omitempty" yaml:"required,omitempty"`
	Purpose    string     `json:"purpose,omitempty" yaml:"purpose,omitempty"`
}

// ProjectionRequest asks the compiler to bind an enabled capability to one
// retrieval namespace.
type ProjectionRequest struct {
	Capability Capability `json:"capability" yaml:"capability"`
	Namespace  string     `json:"namespace" yaml:"namespace"`
	Required   bool       `json:"required,omitempty" yaml:"required,omitempty"`
}

// StageSpec declares a named assembly stage without runtime behavior.
type StageSpec struct {
	Name     string         `json:"name" yaml:"name"`
	Async    bool           `json:"async,omitempty" yaml:"async,omitempty"`
	Optional bool           `json:"optional,omitempty" yaml:"optional,omitempty"`
	Config   map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

// Assembly is the validated internal plan produced from a Spec.
type Assembly struct {
	Sources     []SourceSpec         `json:"sources,omitempty" yaml:"sources,omitempty"`
	Views       []ViewAssembly       `json:"views,omitempty" yaml:"views,omitempty"`
	Projections []ProjectionAssembly `json:"projections,omitempty" yaml:"projections,omitempty"`
	WriteStages []StageSpec          `json:"write_stages,omitempty" yaml:"write_stages,omitempty"`
	ReadStages  []StageSpec          `json:"read_stages,omitempty" yaml:"read_stages,omitempty"`
	Lifecycle   []StageSpec          `json:"lifecycle,omitempty" yaml:"lifecycle,omitempty"`
	Diagnostics []StageSpec          `json:"diagnostics,omitempty" yaml:"diagnostics,omitempty"`
}

// ViewAssembly binds a capability to the descriptor exposed by the view package.
type ViewAssembly struct {
	Capability Capability       `json:"capability" yaml:"capability"`
	Descriptor views.Descriptor `json:"descriptor" yaml:"descriptor"`
	Required   bool             `json:"required,omitempty" yaml:"required,omitempty"`
	Purpose    string           `json:"purpose,omitempty" yaml:"purpose,omitempty"`
}

// ProjectionAssembly binds a capability's record families to a retrieval namespace.
type ProjectionAssembly struct {
	Capability  Capability      `json:"capability" yaml:"capability"`
	RecordTypes []string        `json:"record_types,omitempty" yaml:"recordTypes,omitempty"`
	ViewKind    views.Kind      `json:"view_kind" yaml:"viewKind"`
	Binding     indexed.Binding `json:"binding" yaml:"binding"`
	Required    bool            `json:"required,omitempty" yaml:"required,omitempty"`
	Projectors  []string        `json:"projectors,omitempty" yaml:"projectors,omitempty"`
}

// Compile converts a capability spec into a validated internal assembly plan.
func Compile(spec Spec) (Assembly, error) {
	if err := spec.Validate(); err != nil {
		return Assembly{}, err
	}

	assembly := Assembly{
		Sources:     cloneSourceSpecs(spec.Sources),
		WriteStages: cloneStageSpecs(spec.WriteStages),
		ReadStages:  cloneStageSpecs(spec.ReadStages),
		Lifecycle:   cloneStageSpecs(spec.Lifecycle),
		Diagnostics: cloneStageSpecs(spec.Diagnostics),
		Views:       make([]ViewAssembly, 0, len(spec.Capabilities)),
		Projections: make([]ProjectionAssembly, 0, len(spec.Projections)),
	}

	for _, capability := range spec.Capabilities {
		descriptor, err := descriptorForCapability(capability.Capability)
		if err != nil {
			return Assembly{}, err
		}
		assembly.Views = append(assembly.Views, ViewAssembly{
			Capability: capability.Capability,
			Descriptor: descriptor,
			Required:   capability.Required,
			Purpose:    capability.Purpose,
		})
	}

	for _, request := range spec.Projections {
		template, err := projectionTemplateForCapability(request.Capability)
		if err != nil {
			return Assembly{}, err
		}
		assembly.Projections = append(assembly.Projections, ProjectionAssembly{
			Capability:  request.Capability,
			RecordTypes: append([]string(nil), template.recordTypes...),
			ViewKind:    template.viewKind,
			Binding:     indexed.Binding{Namespace: request.Namespace},
			Required:    request.Required,
			Projectors:  append([]string(nil), template.projectors...),
		})
	}

	if err := assembly.Validate(); err != nil {
		return Assembly{}, err
	}
	return assembly, nil
}

// Validate checks that the requested sources, capabilities, projections, and
// stages can be compiled coherently.
func (s Spec) Validate() error {
	if err := validateSources(s.Sources); err != nil {
		return err
	}
	enabled, err := validateCapabilities(s.Capabilities)
	if err != nil {
		return err
	}
	if err := validateCapabilityDependencies(s.Sources, enabled); err != nil {
		return err
	}
	if err := validateProjectionRequests(s.Projections, enabled); err != nil {
		return err
	}
	if err := validateStages("write_stages", s.WriteStages); err != nil {
		return err
	}
	if err := validateStages("read_stages", s.ReadStages); err != nil {
		return err
	}
	if err := validateStages("lifecycle", s.Lifecycle); err != nil {
		return err
	}
	return validateStages("diagnostics", s.Diagnostics)
}

// Validate checks that the compiled assembly is internally consistent.
func (a Assembly) Validate() error {
	if err := validateSources(a.Sources); err != nil {
		return err
	}
	if err := validateViewAssemblies(a.Views); err != nil {
		return err
	}
	if err := validateProjectionAssemblies(a.Projections); err != nil {
		return err
	}
	if err := validateStages("write_stages", a.WriteStages); err != nil {
		return err
	}
	if err := validateStages("read_stages", a.ReadStages); err != nil {
		return err
	}
	if err := validateStages("lifecycle", a.Lifecycle); err != nil {
		return err
	}
	return validateStages("diagnostics", a.Diagnostics)
}

// HasSource reports whether the compiled assembly declares source.
func (a Assembly) HasSource(source SourceKind) bool {
	for _, compiled := range a.Sources {
		if compiled.Kind == source {
			return true
		}
	}
	return false
}

// HasCapability reports whether the compiled assembly enables capability.
func (a Assembly) HasCapability(capability Capability) bool {
	for _, view := range a.Views {
		if view.Capability == capability {
			return true
		}
	}
	return false
}

// Capabilities returns the capabilities enabled by the compiled assembly.
func (a Assembly) Capabilities() []Capability {
	out := make([]Capability, 0, len(a.Views))
	for _, view := range a.Views {
		out = append(out, view.Capability)
	}
	return out
}

// ProjectionNamespace returns the retrieval namespace bound to capability.
func (a Assembly) ProjectionNamespace(capability Capability) (string, bool) {
	for _, projection := range a.Projections {
		if projection.Capability == capability {
			return projection.Binding.Namespace, true
		}
	}
	return "", false
}

func descriptorForCapability(capability Capability) (views.Descriptor, error) {
	switch capability {
	case CapabilityRecentWindow:
		return viewDescriptor(views.KindRecentWindow, recent.DefaultWindowID, recent.DefaultWindowVersion), nil
	case CapabilitySummaryDAG:
		return viewDescriptor(views.KindSummaryDAG, recent.DefaultSummaryDAGID, recent.DefaultSummaryDAGVersion), nil
	case CapabilityDocumentChunks:
		return viewDescriptor(views.KindDocumentChunks, document.DefaultChunksID, document.DefaultChunksVersion), nil
	case CapabilityMessageIndex:
		return viewDescriptor(views.KindMessageIndex, views.ID("message_index"), "v1"), nil
	case CapabilityEntityFactIndex:
		return viewDescriptor(views.KindEntityFacts, entityfact.DefaultEntityFactsID, entityfact.DefaultEntityFactsVersion), nil
	default:
		return views.Descriptor{}, errdefs.Validationf("%s: unknown capability %q", errPrefix, capability)
	}
}

func viewDescriptor(kind views.Kind, id views.ID, version string) views.Descriptor {
	return views.Descriptor{
		ID:      id,
		Kind:    kind,
		Version: version,
	}
}

type projectionTemplate struct {
	recordTypes []string
	viewKind    views.Kind
	projectors  []string
}

func projectionTemplateForCapability(capability Capability) (projectionTemplate, error) {
	switch capability {
	case CapabilityMessageIndex:
		return projectionTemplate{
			recordTypes: []string{projectors.RecordTypeSourceMessage},
			viewKind:    views.KindMessageIndex,
			projectors:  []string{"SourceMessageRecords"},
		}, nil
	case CapabilityDocumentChunks:
		return projectionTemplate{
			recordTypes: []string{projectors.RecordTypeDocumentChunk},
			viewKind:    views.KindDocumentChunks,
			projectors:  []string{"DocumentChunk"},
		}, nil
	case CapabilitySummaryDAG:
		return projectionTemplate{
			recordTypes: []string{projectors.RecordTypeSummaryNode},
			viewKind:    views.KindSummaryDAG,
			projectors:  []string{"SummaryNode"},
		}, nil
	case CapabilityEntityFactIndex:
		return projectionTemplate{
			recordTypes: []string{projectors.RecordTypeEntityFact},
			viewKind:    views.KindEntityFacts,
			projectors:  []string{"EntityFact"},
		}, nil
	default:
		return projectionTemplate{}, errdefs.Validationf("%s: capability %q does not support indexed projection", errPrefix, capability)
	}
}

func validateSources(specs []SourceSpec) error {
	if len(specs) == 0 {
		return errdefs.Validationf("%s: at least one source is required", errPrefix)
	}

	seen := make(map[SourceKind]struct{}, len(specs))
	for i, spec := range specs {
		if !knownSourceKind(spec.Kind) {
			return errdefs.Validationf("%s: sources[%d] has unknown kind %q", errPrefix, i, spec.Kind)
		}
		if _, ok := seen[spec.Kind]; ok {
			return errdefs.Validationf("%s: duplicate source kind %q", errPrefix, spec.Kind)
		}
		seen[spec.Kind] = struct{}{}
	}
	return nil
}

func validateCapabilities(specs []CapabilitySpec) (map[Capability]CapabilitySpec, error) {
	seen := make(map[Capability]CapabilitySpec, len(specs))
	for i, spec := range specs {
		if !knownCapability(spec.Capability) {
			return nil, errdefs.Validationf("%s: capabilities[%d] has unknown capability %q", errPrefix, i, spec.Capability)
		}
		if _, ok := seen[spec.Capability]; ok {
			return nil, errdefs.Validationf("%s: duplicate capability %q", errPrefix, spec.Capability)
		}
		seen[spec.Capability] = spec
	}
	return seen, nil
}

func validateCapabilityDependencies(sources []SourceSpec, capabilities map[Capability]CapabilitySpec) error {
	enabledSources := make(map[SourceKind]struct{}, len(sources))
	for _, source := range sources {
		enabledSources[source.Kind] = struct{}{}
	}

	if err := requireSource(capabilities, CapabilityRecentWindow, enabledSources, SourceMessageLog); err != nil {
		return err
	}
	if err := requireSource(capabilities, CapabilitySummaryDAG, enabledSources, SourceMessageLog); err != nil {
		return err
	}
	if err := requireSource(capabilities, CapabilityDocumentChunks, enabledSources, SourceDocumentStore); err != nil {
		return err
	}
	if err := requireSource(capabilities, CapabilityMessageIndex, enabledSources, SourceMessageLog); err != nil {
		return err
	}
	if err := requireSource(capabilities, CapabilityEntityFactIndex, enabledSources, SourceMessageLog); err != nil {
		return err
	}
	return nil
}

func requireSource(capabilities map[Capability]CapabilitySpec, capability Capability, sources map[SourceKind]struct{}, source SourceKind) error {
	if _, ok := capabilities[capability]; !ok {
		return nil
	}
	if _, ok := sources[source]; ok {
		return nil
	}
	return errdefs.Validationf("%s: capability %q requires source %q", errPrefix, capability, source)
}

func validateProjectionRequests(requests []ProjectionRequest, enabled map[Capability]CapabilitySpec) error {
	seenNamespaces := make(map[string]struct{}, len(requests))
	for i, request := range requests {
		if !knownCapability(request.Capability) {
			return errdefs.Validationf("%s: projections[%d] has unknown capability %q", errPrefix, i, request.Capability)
		}
		if _, ok := enabled[request.Capability]; !ok {
			return errdefs.Validationf("%s: projections[%d] capability %q is not enabled", errPrefix, i, request.Capability)
		}
		if _, err := projectionTemplateForCapability(request.Capability); err != nil {
			return errdefs.Validationf("%s: projections[%d] unsupported capability: %w", errPrefix, i, err)
		}
		binding := indexed.Binding{Namespace: request.Namespace}
		if err := binding.Validate(); err != nil {
			return errdefs.Validationf("%s: projections[%d] invalid binding: %w", errPrefix, i, err)
		}
		if _, ok := seenNamespaces[request.Namespace]; ok {
			return errdefs.Validationf("%s: duplicate projection namespace %q", errPrefix, request.Namespace)
		}
		seenNamespaces[request.Namespace] = struct{}{}
	}
	return nil
}

func validateViewAssemblies(assemblies []ViewAssembly) error {
	seenCapabilities := make(map[Capability]struct{}, len(assemblies))
	seenIDs := make(map[views.ID]struct{}, len(assemblies))
	for i, assembly := range assemblies {
		if !knownCapability(assembly.Capability) {
			return errdefs.Validationf("%s: views[%d] has unknown capability %q", errPrefix, i, assembly.Capability)
		}
		if err := assembly.Descriptor.Validate(); err != nil {
			return errdefs.Validationf("%s: views[%d] invalid descriptor: %w", errPrefix, i, err)
		}
		if _, ok := seenCapabilities[assembly.Capability]; ok {
			return errdefs.Validationf("%s: duplicate view capability %q", errPrefix, assembly.Capability)
		}
		if _, ok := seenIDs[assembly.Descriptor.ID]; ok {
			return errdefs.Validationf("%s: duplicate view id %q", errPrefix, assembly.Descriptor.ID)
		}
		seenCapabilities[assembly.Capability] = struct{}{}
		seenIDs[assembly.Descriptor.ID] = struct{}{}
	}
	return nil
}

func validateProjectionAssemblies(assemblies []ProjectionAssembly) error {
	seenNamespaces := make(map[string]struct{}, len(assemblies))
	for i, assembly := range assemblies {
		template, err := projectionTemplateForCapability(assembly.Capability)
		if err != nil {
			return errdefs.Validationf("%s: projections[%d] unsupported capability: %w", errPrefix, i, err)
		}
		if assembly.ViewKind != template.viewKind {
			return errdefs.Validationf("%s: projections[%d] view kind %q does not match capability %q", errPrefix, i, assembly.ViewKind, assembly.Capability)
		}
		if err := validateStrings("record_types", i, assembly.RecordTypes); err != nil {
			return err
		}
		if err := validateStrings("projectors", i, assembly.Projectors); err != nil {
			return err
		}
		if err := assembly.Binding.Validate(); err != nil {
			return errdefs.Validationf("%s: projections[%d] invalid binding: %w", errPrefix, i, err)
		}
		if _, ok := seenNamespaces[assembly.Binding.Namespace]; ok {
			return errdefs.Validationf("%s: duplicate projection namespace %q", errPrefix, assembly.Binding.Namespace)
		}
		seenNamespaces[assembly.Binding.Namespace] = struct{}{}
	}
	return nil
}

func validateStrings(field string, projectionIndex int, values []string) error {
	if len(values) == 0 {
		return errdefs.Validationf("%s: projections[%d] %s are required", errPrefix, projectionIndex, field)
	}
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if value == "" {
			return errdefs.Validationf("%s: projections[%d] %s[%d] is required", errPrefix, projectionIndex, field, i)
		}
		if _, ok := seen[value]; ok {
			return errdefs.Validationf("%s: projections[%d] duplicate %s value %q", errPrefix, projectionIndex, field, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateStages(listName string, specs []StageSpec) error {
	seen := make(map[string]struct{}, len(specs))
	for i, spec := range specs {
		if spec.Name == "" {
			return errdefs.Validationf("%s: %s[%d] name is required", errPrefix, listName, i)
		}
		if _, ok := seen[spec.Name]; ok {
			return errdefs.Validationf("%s: duplicate %s stage %q", errPrefix, listName, spec.Name)
		}
		seen[spec.Name] = struct{}{}
	}
	return nil
}

func knownSourceKind(kind SourceKind) bool {
	switch kind {
	case SourceMessageLog, SourceDocumentStore:
		return true
	default:
		return false
	}
}

func knownCapability(capability Capability) bool {
	switch capability {
	case CapabilityRecentWindow,
		CapabilitySummaryDAG,
		CapabilityDocumentChunks,
		CapabilityMessageIndex,
		CapabilityEntityFactIndex:
		return true
	default:
		return false
	}
}

func cloneSourceSpecs(in []SourceSpec) []SourceSpec {
	if in == nil {
		return nil
	}
	out := make([]SourceSpec, len(in))
	copy(out, in)
	return out
}

func cloneStageSpecs(in []StageSpec) []StageSpec {
	if in == nil {
		return nil
	}
	out := make([]StageSpec, len(in))
	for i, spec := range in {
		out[i] = spec
		out[i].Config = maps.Clone(spec.Config)
	}
	return out
}
