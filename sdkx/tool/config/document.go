package config

import (
	"bytes"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
	yamlv3 "gopkg.in/yaml.v3"
)

// VersionV1 is the only supported document version.
const VersionV1 = "v1"

// Document is the parsed tool-execution policy.
type Document struct {
	Version string `yaml:"version"`
	// Sources declares external tool providers to attach before the
	// chain is built, so their tools are registered by the time scopes
	// and middleware reference them.
	Sources     []SourceEntry     `yaml:"sources,omitempty"`
	Middlewares []MiddlewareEntry `yaml:"middlewares,omitempty"`
	Scopes      map[string]string `yaml:"scopes,omitempty"`
}

// MiddlewareEntry is one chain link: a factory kind plus its opaque
// spec. Spec stays an undecoded YAML subtree so each factory decodes
// exactly the fields it owns, strictly.
type MiddlewareEntry struct {
	Kind string  `yaml:"kind"`
	Spec *Opaque `yaml:"spec,omitempty"`
}

// Opaque captures a factory-owned YAML subtree without decoding it.
// Implementing UnmarshalYAML stops the document's KnownFields(true)
// strictness from recursing into a spec whose schema this package does
// not know, while every field it does own stays strictly checked.
type Opaque yamlv3.Node

// UnmarshalYAML stores the subtree verbatim.
func (o *Opaque) UnmarshalYAML(node *yamlv3.Node) error {
	*o = Opaque(*node)
	return nil
}

// Node returns the captured subtree for [DecodeSpec].
func (o *Opaque) Node() *yamlv3.Node {
	if o == nil {
		return nil
	}
	return (*yamlv3.Node)(o)
}

// Validate checks the document's own invariants. Factory-level spec
// validation happens in Build, where the factories live.
func (d Document) Validate() error {
	if d.Version != VersionV1 {
		return errdefs.Validation(fmt.Errorf(
			"tool config version %q is not supported (want %q)",
			d.Version, VersionV1))
	}
	for i, entry := range d.Sources {
		if entry.Kind == "" {
			return errdefs.Validation(fmt.Errorf(
				"tool config sources[%d]: kind is required", i))
		}
	}
	for i, entry := range d.Middlewares {
		if entry.Kind == "" {
			return errdefs.Validation(fmt.Errorf(
				"tool config middlewares[%d]: kind is required", i))
		}
	}
	for name, scope := range d.Scopes {
		if name == "" {
			return errdefs.Validation(fmt.Errorf(
				"tool config scopes: empty tool name"))
		}
		if scope != tool.ScopeAgent && scope != tool.ScopePlatform {
			return errdefs.Validation(fmt.Errorf(
				"tool config scopes[%q]: scope %q is not %q or %q",
				name, scope, tool.ScopeAgent, tool.ScopePlatform))
		}
	}
	return nil
}

// Parse decodes strict YAML into a validated Document: unknown
// fields, trailing documents, and invalid values are all errors.
//
// Specs are carried as YAML nodes rather than converted to JSON, so
// any legal YAML a factory understands survives — timestamps, multi-line
// block scalars, aliases — and there is only one encoding to reason
// about.
func Parse(data []byte) (Document, error) {
	decoder := yamlv3.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, errdefs.Validation(fmt.Errorf(
			"decode tool config YAML: %w", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents")
		}
		return Document{}, errdefs.Validation(fmt.Errorf(
			"decode tool config YAML: %w", err))
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// DecodeSpec strictly decodes a factory's spec subtree into T: unknown
// keys are errors, so a YAML typo fails the build instead of silently
// dropping policy. Factories SHOULD decode through this helper.
//
// A nil node is an error: a factory calling DecodeSpec has fields it
// needs. Kinds that take no spec reject one explicitly instead.
func DecodeSpec[T any](node *yamlv3.Node) (T, error) {
	var out T
	if node == nil {
		return out, errdefs.Validation(fmt.Errorf("spec is required"))
	}
	raw, err := yamlv3.Marshal(node)
	if err != nil {
		return out, errdefs.Validation(fmt.Errorf(
			"re-encode spec node: %w", err))
	}
	decoder := yamlv3.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&out); err != nil {
		return out, errdefs.Validation(fmt.Errorf("invalid spec: %w", err))
	}
	return out, nil
}

// isEmptySpec reports whether a spec node carries no content, which is
// how a "takes no spec" kind distinguishes an absent spec from one an
// author wrote by mistake.
func isEmptySpec(node *yamlv3.Node) bool {
	if node == nil {
		return true
	}
	switch node.Kind {
	case yamlv3.MappingNode, yamlv3.SequenceNode:
		return len(node.Content) == 0
	case yamlv3.ScalarNode:
		return node.Tag == "!!null"
	default:
		return false
	}
}
