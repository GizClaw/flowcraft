package deploy

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	yamlv3 "gopkg.in/yaml.v3"
)

// VersionV1 is the only supported document version.
const VersionV1 = "v1"

// RefSeparator splits a resource name from an item name inside it in
// the scalar dep form ("fs/project"). Slash is safe because resource
// and item names are identifiers: dots are taken by kind names
// (inference.Runtime) and slashes appear in neither.
const RefSeparator = "/"

// Document is the deployment file, decoded and self-validated. The Go
// types ARE the v1 wire format — yaml tags are the single source of
// truth. Kind/type/source existence is NOT checked here; that
// requires the Builder's registries.
type Document struct {
	Version string `yaml:"version"`

	// Resources is the resource area: named, shared, long-lived
	// objects built once per Build and handed to whatever binds them.
	// Each entry names a (kind, impl) constructor registered on the
	// Builder, so a module's own config package — inference, tool,
	// workspace, sandbox — plugs in as one impl without this package
	// importing it.
	//
	// Resources may depend on each other through [ResourceEntry.Deps];
	// Build orders construction topologically.
	Resources map[string]ResourceEntry `yaml:"resources,omitempty"`

	Agents map[string]AgentEntry `yaml:"agents"`
}

// ResourceEntry is one shared resource definition: a category kind
// (matched against DepSpec.Type when a whole resource is bound), an
// implementation selector, that impl's dependencies on other
// resources, and its opaque settings subtree (strictly decoded by the
// registered ResourceFunc).
type ResourceEntry struct {
	Kind string `yaml:"kind"`
	Impl string `yaml:"impl"`

	// Deps binds constructor-defined dependency names to other
	// resources (or host sources). The constructor type-asserts each
	// value: unlike an engine, a resource has no static DepSpec list
	// to validate against.
	Deps map[string]DepRef `yaml:"deps,omitempty"`

	Settings *Opaque `yaml:"settings,omitempty"`
}

// AgentEntry is one agent's declarative assembly recipe. The map key
// in [Document.Agents] becomes Agent.ID.
type AgentEntry struct {
	// Card is the declarative subset of agent.AgentCard.
	Card struct {
		Name        string `yaml:"name,omitempty"`
		Description string `yaml:"description,omitempty"`
	} `yaml:"card,omitempty"`

	// Tools is the agent-level tool allow-list (policy gate,
	// promoted to Run.ToolAllowList by the harness).
	Tools []string `yaml:"tools,omitempty"`

	// Engine selects an engine kind from the agent.Registry.
	Engine EngineEntry `yaml:"engine"`

	// Deps binds EngineSpec-declared dep names to resources or
	// sources.
	Deps map[string]DepRef `yaml:"deps,omitempty"`

	// Before is the optional seed hook (singular).
	Prepare []PreparerEntry `yaml:"prepare,omitempty"`

	// Hooks are lifecycle observers, fired in document order.
	Observe []ObserverEntry `yaml:"observe,omitempty"`

	// After are decision hooks, merged in document order.
	Referees []RefereeEntry `yaml:"referees,omitempty"`

	// Policy is the per-call harness policy.
	Policy struct {
		MaxRevise        int      `yaml:"max_revise,omitempty"`
		ArtifactChannels []string `yaml:"artifact_channels,omitempty"`
	} `yaml:"policy,omitempty"`
}

// EngineEntry selects an engine kind and carries its kind-specific
// settings as pure YAML-decoded data, opaque to the loader — the
// engine factory decodes and strictly validates it inside New.
type EngineEntry struct {
	Kind     string         `yaml:"kind"`
	Settings map[string]any `yaml:"settings,omitempty"`
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
	Source   string `yaml:"source,omitempty"`
	Resource string `yaml:"resource,omitempty"`
	Ref      string `yaml:"ref,omitempty"`
}

// UnmarshalYAML accepts either the scalar shorthand ("infer",
// "fs/project") or the explicit mapping. A scalar always means a
// resource: sources are rare and deliberately verbose.
func (d *DepRef) UnmarshalYAML(node *yamlv3.Node) error {
	if node.Kind == yamlv3.ScalarNode {
		var text string
		if err := node.Decode(&text); err != nil {
			return err
		}
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
	// re-entered. Node.Decode ignores unknown keys, so route through
	// DecodeSettings to keep a misspelled "resouce:" a build error
	// rather than a silently unbound dep.
	type depRefWire DepRef
	wire, err := DecodeSettings[depRefWire](node)
	if err != nil {
		return err
	}
	*d = DepRef(wire)
	return nil
}

// PreparerEntry is one link in the prepare chain: a factory type,
// its dependencies, and its opaque factory-owned settings subtree.
// All three lifecycle stages — prepare / observe / referee — share
// the same data shape; the three names exist so the type system
// catches a factory registered against the wrong stage.
type PreparerEntry struct {
	Type string `yaml:"type"`

	// Deps binds factory-defined dependency names to resources or
	// sources, so a Preparer can reach a store the resource area
	// built.
	Deps map[string]DepRef `yaml:"deps,omitempty"`

	Settings *Opaque `yaml:"settings,omitempty"`
}

// ObserverEntry is one read-only lifecycle observer.
type ObserverEntry = PreparerEntry

// RefereeEntry is one decision hook.
type RefereeEntry = PreparerEntry

// Opaque captures a YAML subtree without decoding it. Implementing
// UnmarshalYAML stops the document's KnownFields(true) strictness
// from recursing into factory-owned payloads, while every other
// field in the document stays strictly checked.
type Opaque yamlv3.Node

// UnmarshalYAML stores the subtree verbatim.
func (o *Opaque) UnmarshalYAML(node *yamlv3.Node) error {
	*o = Opaque(*node)
	return nil
}

// Node returns the captured subtree as a yamlv3.Node for
// [DecodeSettings].
func (o *Opaque) Node() *yamlv3.Node {
	if o == nil {
		return nil
	}
	return (*yamlv3.Node)(o)
}

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
		if a.Engine.Kind == "" {
			return errdefs.Validation(fmt.Errorf(
				"deploy config agents[%q]: engine.kind is required", id))
		}
		if err := validateDeps(a.Deps); err != nil {
			return errdefs.Validation(fmt.Errorf(
				"deploy config agents[%q]: %w", id, err))
		}
		for i, p := range a.Prepare {
			if err := p.validate(); err != nil {
				return errdefs.Validation(fmt.Errorf(
					"deploy config agents[%q].prepare[%d]: %w", id, i, err))
			}
		}
		for i, o := range a.Observe {
			if err := o.validate(); err != nil {
				return errdefs.Validation(fmt.Errorf(
					"deploy config agents[%q].observe[%d]: %w", id, i, err))
			}
		}
		for i, r := range a.Referees {
			if err := r.validate(); err != nil {
				return errdefs.Validation(fmt.Errorf(
					"deploy config agents[%q].referees[%d]: %w", id, i, err))
			}
		}
		if a.Policy.MaxRevise < 0 {
			return errdefs.Validation(fmt.Errorf(
				"deploy config agents[%q].policy.max_revise: must be >= 0", id))
		}
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

// Parse decodes strict YAML into a validated Document: unknown
// fields, trailing documents, and invalid values are all errors.
func Parse(data []byte) (Document, error) {
	decoder := yamlv3.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, errdefs.Validation(fmt.Errorf(
			"decode deploy config YAML: %w", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents")
		}
		return Document{}, errdefs.Validation(fmt.Errorf(
			"decode deploy config YAML: %w", err))
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// DecodeSettings decodes an opaque settings subtree into T with
// KnownFields strictness: unknown keys are errors, so a YAML typo
// fails the build instead of silently dropping policy. Every
// resource / hook / before / after factory SHOULD decode through
// this helper.
//
// A nil node decodes as the zero value of T.
func DecodeSettings[T any](node *yamlv3.Node) (T, error) {
	var out T
	if node == nil {
		return out, nil
	}
	raw, err := yamlv3.Marshal(node)
	if err != nil {
		return out, fmt.Errorf("re-encode settings node: %w", err)
	}
	decoder := yamlv3.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}
