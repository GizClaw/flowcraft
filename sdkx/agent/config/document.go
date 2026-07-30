package config

import (
	"bytes"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	yamlv3 "gopkg.in/yaml.v3"
)

// VersionV1 is the only supported document version.
const VersionV1 = "v1"

// Document is the agent config file, decoded and self-validated.
// The Go types ARE the v1 wire format — yaml tags are the single
// source of truth for both. Kind/type/source existence is NOT
// checked here; that requires the Builder's registries.
type Document struct {
	Version string                `yaml:"version"`
	Agents  map[string]AgentEntry `yaml:"agents"`
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

	// Deps binds EngineSpec-declared dep names to source references.
	Deps map[string]DepRef `yaml:"deps,omitempty"`

	// Before is the optional seed hook (singular).
	Before *HookEntry `yaml:"before,omitempty"`

	// Hooks are lifecycle observers, fired in document order.
	Hooks []HookEntry `yaml:"hooks,omitempty"`

	// After are decision hooks, merged in document order.
	After []HookEntry `yaml:"after,omitempty"`

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

// DepRef binds one DepSpec-named dependency to a value resolved by
// a named application source (e.g. source "inference.profile",
// ref "kimi-k2"). Ref is optional for singleton sources.
type DepRef struct {
	Source string `yaml:"source"`
	Ref    string `yaml:"ref,omitempty"`
}

// HookEntry is one lifecycle extension point: a factory type plus
// its opaque, factory-owned settings subtree.
type HookEntry struct {
	Type     string  `yaml:"type"`
	Settings *Opaque `yaml:"settings,omitempty"`
}

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
			"agent config version %q is not supported (want %q)",
			d.Version, VersionV1))
	}
	for id, a := range d.Agents {
		if id == "" {
			return errdefs.Validation(fmt.Errorf(
				"agent config agents: empty agent id"))
		}
		if a.Engine.Kind == "" {
			return errdefs.Validation(fmt.Errorf(
				"agent config agents[%q]: engine.kind is required", id))
		}
		for name, dep := range a.Deps {
			if name == "" {
				return errdefs.Validation(fmt.Errorf(
					"agent config agents[%q].deps: empty dep name", id))
			}
			if dep.Source == "" {
				return errdefs.Validation(fmt.Errorf(
					"agent config agents[%q].deps[%q]: source is required", id, name))
			}
		}
		for i, h := range a.Hooks {
			if h.Type == "" {
				return errdefs.Validation(fmt.Errorf(
					"agent config agents[%q].hooks[%d]: type is required", id, i))
			}
		}
		if a.Before != nil && a.Before.Type == "" {
			return errdefs.Validation(fmt.Errorf(
				"agent config agents[%q].before: type is required", id))
		}
		for i, h := range a.After {
			if h.Type == "" {
				return errdefs.Validation(fmt.Errorf(
					"agent config agents[%q].after[%d]: type is required", id, i))
			}
		}
		if a.Policy.MaxRevise < 0 {
			return errdefs.Validation(fmt.Errorf(
				"agent config agents[%q].policy.max_revise: must be >= 0", id))
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
			"decode agent config YAML: %w", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents")
		}
		return Document{}, errdefs.Validation(fmt.Errorf(
			"decode agent config YAML: %w", err))
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// DecodeSettings decodes a hook's opaque settings subtree into T
// with KnownFields strictness: unknown keys are errors, so a YAML
// typo fails the build instead of silently dropping policy. Every
// hook / before / after factory SHOULD decode through this helper.
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
