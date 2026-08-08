package deploy

import (
	"encoding/json"
	"fmt"
	"strings"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/config/utils"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// VersionV1 is the only supported document version.
const VersionV1 = "v1"

// RefSeparator splits a resource name from an item name inside it in
// the scalar dep form ("fs/project"). Slash is safe because resource
// and item names are identifiers: dots are taken by kind names
// (inference.Runtime) and slashes appear in neither.
const RefSeparator = "/"

// Document is the deployment file, decoded and self-validated. The Go
// types ARE the v1 wire format — json tags are the single source of
// truth; YAML is authoring sugar converted before decoding by
// sdk/config/utils. Kind/type/source existence is NOT checked here;
// that requires the Builder's registries.
type Document struct {
	Version string `json:"version"`

	// Resources is the resource area: named, shared, long-lived
	// objects built once per Build and handed to whatever binds them.
	// Each entry names a (kind, impl) factory registered on the
	// Builder, so a module's own config package — inference, tool,
	// workspace, sandbox — plugs in as one impl without this package
	// importing it.
	//
	// Resources may depend on each other through [ResourceEntry.Deps];
	// Build orders construction topologically.
	Resources map[string]ResourceEntry `json:"resources,omitempty"`

	Agents map[string]AgentEntry `json:"agents"`

	// Runtime is an application-runtime-owned configuration subtree.
	// Parse preserves it verbatim so the runtime layer can decode it
	// strictly without weakening strictness for the deployment schema.
	Runtime *sdkconfig.Opaque `json:"runtime,omitempty"`
}

// ResourceEntry is one shared resource definition: a category kind
// (matched against DepSpec.Type when a whole resource is bound), an
// implementation selector, that impl's dependencies on other
// resources, and its opaque settings subtree (strictly decoded by the
// registered ResourceFactory).
type ResourceEntry struct {
	Kind string `json:"kind"`
	Impl string `json:"impl"`

	// Export keeps a resource as an application-facing root even when no
	// agent, hook, or other resource binds it. The application retrieves
	// the value through ResourceAs and owns it through Result.Close.
	Export bool `json:"export,omitempty"`

	// Deps binds factory-declared dependency names to other resources
	// (or host sources). Build validates these names and required
	// bindings against ResourceSpec.Deps before calling the factory.
	Deps map[string]DepRef `json:"deps,omitempty"`

	Settings json.RawMessage `json:"settings,omitempty"`
}

// AgentEntry is one agent's declarative assembly recipe. The map key
// in [Document.Agents] becomes Agent.ID. An entry may instead set File
// to load the recipe during [Builder.Build]; File and the inline fields
// are mutually exclusive.
type AgentEntry struct {
	// Source is the recipe location: {"file": "./x.yaml"} or
	// {"embed": "name"}. Omitted, the entry is inline (the fields
	// below). A recipe file never references another recipe file.
	Source *sdkconfig.Ref `json:"source,omitempty"`

	// Card is the declarative subset of agent.AgentCard.
	Card struct {
		Name        string `json:"name,omitempty"`
		Description string `json:"description,omitempty"`
	} `json:"card,omitempty"`

	// Tools is the agent-level tool allow-list (policy gate,
	// promoted to Run.ToolAllowList by the harness).
	Tools []string `json:"tools,omitempty"`

	// Engine selects an engine kind from the agent.Registry.
	Engine EngineEntry `json:"engine"`

	// Deps binds EngineSpec-declared dep names to resources or
	// sources.
	Deps map[string]DepRef `json:"deps,omitempty"`

	// Prepare is the optional seed hook chain.
	Prepare []PreparerEntry `json:"prepare,omitempty"`

	// Observe are lifecycle observers, fired in document order.
	Observe []ObserverEntry `json:"observe,omitempty"`

	// Referees are decision hooks, merged in document order.
	Referees []RefereeEntry `json:"referees,omitempty"`

	// Commit contains durable finalizers, run in document order after
	// Referees accept the final result and before Observers finish it.
	Commit []CommitterEntry `json:"commit,omitempty"`

	// Policy is the per-call harness policy.
	Policy struct {
		MaxRevise        int      `json:"max_revise,omitempty"`
		ArtifactChannels []string `json:"artifact_channels,omitempty"`
	} `json:"policy,omitempty"`

	source         agentEntrySource
	inlineDeclared bool
}

type agentEntrySource uint8

const (
	agentEntrySourceDirect agentEntrySource = iota
	agentEntrySourceInline
	agentEntrySourceRef
)

type agentEntryWire AgentEntry

// UnmarshalJSON records whether the mapping selected a file or inline
// source while retaining strict decoding for the complete AgentEntry
// schema. Presence is recorded separately from values so file plus an
// empty-but-declared inline field is still rejected.
func (a *AgentEntry) UnmarshalJSON(data []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	hasSource := false
	if raw, ok := probe["source"]; ok && string(raw) != "null" {
		hasSource = true
	}
	hasInline := false
	for _, key := range []string{
		"card", "tools", "engine", "deps", "prepare",
		"observe", "referees", "commit", "policy",
	} {
		if raw, ok := probe[key]; ok && string(raw) != "null" {
			hasInline = true
			break
		}
	}

	wire, err := sdkconfig.DecodeSettings[agentEntryWire](data)
	if err != nil {
		return err
	}
	*a = AgentEntry(wire)
	a.inlineDeclared = hasInline
	switch {
	case hasSource:
		a.source = agentEntrySourceRef
	case hasInline:
		a.source = agentEntrySourceInline
	}
	return nil
}

// EngineEntry selects an engine kind and carries its kind-specific
// settings as JSON-decoded data, opaque to the loader — the engine
// factory decodes and strictly validates it inside New.
type EngineEntry struct {
	Kind string `json:"kind"`

	// Settings is the engine-kind-owned subtree as raw JSON. It is
	// opaque to the loader; the engine factory decodes and strictly
	// validates it inside New (the graph engine's settings carry the
	// definition Source, script runtime name, and build knobs).
	Settings json.RawMessage `json:"settings,omitempty"`
}

// DepRef binds one named dependency to a value. Exactly one of
// Source / Resource is set.
//
// The scalar form covers the common cases and is what documents
// should use:
//
//	deps:
//	  inference: infer          # whole resource
//	  workspace: fs/project     # one item inside a container resource
//
// The mapping form is for values the document does not own — an
// instance the host built and will close itself:
//
//	deps:
//	  tools: {source: host.tools, ref: default}
//
// That distinction is the point of having both: a Resource is
// constructed by Build and closed by [Result.Close], while a Source
// is merely borrowed. Binding a host singleton as a resource would
// hand its lifetime to a document that does not own it.
type DepRef struct {
	Source   string `json:"source,omitempty"`
	Resource string `json:"resource,omitempty"`
	Ref      string `json:"ref,omitempty"`
}

// UnmarshalJSON accepts either the scalar shorthand ("infer",
// "fs/project") or the explicit mapping. A scalar always means a
// resource: sources are rare and deliberately verbose.
func (d *DepRef) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return fmt.Errorf("dep reference is empty")
		}
		resource, ref, found := strings.Cut(text, RefSeparator)
		if resource == "" || (found && ref == "") {
			return fmt.Errorf(
				"dep reference %q must be \"resource\" or \"resource%sitem\"",
				text, RefSeparator)
		}
		if strings.Contains(ref, RefSeparator) {
			return fmt.Errorf(
				"dep reference %q has more than one %q separator", text, RefSeparator)
		}
		*d = DepRef{Resource: resource, Ref: ref}
		return nil
	}

	// Mapping form: decode through a twin type so this method is not
	// re-entered, and keep strictness so a misspelled "resouce:" is a
	// build error rather than a silently unbound dep.
	type depRefWire DepRef
	wire, err := sdkconfig.DecodeSettings[depRefWire](data)
	if err != nil {
		return err
	}
	*d = DepRef(wire)
	return nil
}

// PreparerEntry is one link in the prepare chain: a factory type,
// its dependencies, and its opaque factory-owned settings subtree.
// All lifecycle stages — prepare / referee / commit / observe — share
// the same data shape; the distinct names exist so the type system
// catches a factory registered against the wrong stage.
type PreparerEntry struct {
	Type string `json:"type"`

	// Deps binds factory-defined dependency names to resources or
	// sources, so a Preparer can reach a store the resource area
	// built.
	Deps map[string]DepRef `json:"deps,omitempty"`

	Settings json.RawMessage `json:"settings,omitempty"`
}

// ObserverEntry is one read-only lifecycle observer.
type ObserverEntry = PreparerEntry

// RefereeEntry is one decision hook.
type RefereeEntry = PreparerEntry

// CommitterEntry is one durable finalizer.
type CommitterEntry = PreparerEntry

// Validate checks the document's own invariants.
func (d Document) Validate() error {
	if d.Version != VersionV1 {
		return errdefs.Validation(fmt.Errorf(
			"deploy config version %q is not supported (want %q)",
			d.Version, VersionV1))
	}
	for name, r := range d.Resources {
		if name == "" {
			return errdefs.Validation(fmt.Errorf(
				"deploy config resources: empty resource name"))
		}
		if strings.Contains(name, RefSeparator) {
			return errdefs.Validation(fmt.Errorf(
				"deploy config resources[%q]: name must not contain %q",
				name, RefSeparator))
		}
		if r.Kind == "" {
			return errdefs.Validation(fmt.Errorf(
				"deploy config resources[%q]: kind is required", name))
		}
		if r.Impl == "" {
			return errdefs.Validation(fmt.Errorf(
				"deploy config resources[%q]: impl is required", name))
		}
		if err := validateDeps(r.Deps); err != nil {
			return errdefs.Validation(fmt.Errorf(
				"deploy config resources[%q]: %w", name, err))
		}
	}
	for id, a := range d.Agents {
		if id == "" {
			return errdefs.Validation(fmt.Errorf(
				"deploy config agents: empty agent id"))
		}
		where := fmt.Sprintf("deploy config agents[%q]", id)
		if err := validateAgentSource(a); err != nil {
			return errdefs.Validation(fmt.Errorf("%s: %w", where, err))
		}
		if a.usesSource() {
			continue
		}
		if err := validateAgentEntry(a, where); err != nil {
			return err
		}
	}
	return nil
}

func (a AgentEntry) usesSource() bool {
	return a.source == agentEntrySourceRef || a.Source != nil
}

func validateAgentSource(a AgentEntry) error {
	if a.Source != nil {
		if _, ok := a.Source.File(); !ok {
			if _, ok := a.Source.Embed(); !ok {
				return fmt.Errorf("source is required")
			}
		}
	}
	if a.usesSource() && (a.inlineDeclared || hasInlineAgentValues(a)) {
		return fmt.Errorf("source and inline agent fields are mutually exclusive")
	}
	return nil
}

func hasInlineAgentValues(a AgentEntry) bool {
	return a.Card.Name != "" ||
		a.Card.Description != "" ||
		len(a.Tools) != 0 ||
		a.Engine.Kind != "" ||
		len(a.Engine.Settings) != 0 ||
		len(a.Deps) != 0 ||
		len(a.Prepare) != 0 ||
		len(a.Observe) != 0 ||
		len(a.Referees) != 0 ||
		len(a.Commit) != 0 ||
		a.Policy.MaxRevise != 0 ||
		len(a.Policy.ArtifactChannels) != 0
}

func validateAgentEntry(a AgentEntry, where string) error {
	if a.Engine.Kind == "" {
		return errdefs.Validation(fmt.Errorf(
			"%s: engine.kind is required", where))
	}
	if err := validateDeps(a.Deps); err != nil {
		return errdefs.Validation(fmt.Errorf("%s: %w", where, err))
	}
	for i, p := range a.Prepare {
		if err := p.validate(); err != nil {
			return errdefs.Validation(fmt.Errorf(
				"%s.prepare[%d]: %w", where, i, err))
		}
	}
	for i, o := range a.Observe {
		if err := o.validate(); err != nil {
			return errdefs.Validation(fmt.Errorf(
				"%s.observe[%d]: %w", where, i, err))
		}
	}
	for i, r := range a.Referees {
		if err := r.validate(); err != nil {
			return errdefs.Validation(fmt.Errorf(
				"%s.referees[%d]: %w", where, i, err))
		}
	}
	for i, c := range a.Commit {
		if err := c.validate(); err != nil {
			return errdefs.Validation(fmt.Errorf(
				"%s.commit[%d]: %w", where, i, err))
		}
	}
	if a.Policy.MaxRevise < 0 {
		return errdefs.Validation(fmt.Errorf(
			"%s.policy.max_revise: must be >= 0", where))
	}
	return nil
}

func (h PreparerEntry) validate() error {
	if h.Type == "" {
		return fmt.Errorf("type is required")
	}
	return validateDeps(h.Deps)
}

// validateDeps enforces the shape every dep map shares: named, and
// bound to exactly one of source / resource.
func validateDeps(deps map[string]DepRef) error {
	for name, dep := range deps {
		if name == "" {
			return fmt.Errorf("deps: empty dep name")
		}
		switch {
		case dep.Source == "" && dep.Resource == "":
			return fmt.Errorf("deps[%q]: source or resource is required", name)
		case dep.Source != "" && dep.Resource != "":
			return fmt.Errorf(
				"deps[%q]: source and resource are mutually exclusive", name)
		}
	}
	return nil
}

// Parse decodes strict YAML or JSON into a validated Document:
// unknown fields, trailing documents, and invalid values are all
// errors. YAML is accepted as authoring sugar.
func Parse(data []byte) (Document, error) {
	doc, err := utils.Decode[Document](data)
	if err != nil {
		return Document{}, errdefs.Validation(fmt.Errorf(
			"decode deploy config: %w", err))
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// DecodeSettings strictly decodes an opaque settings subtree into T:
// unknown keys are errors. A nil subtree produces the zero value of T.
func DecodeSettings[T any](settings *sdkconfig.Opaque) (T, error) {
	var out T
	if err := settings.Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}
