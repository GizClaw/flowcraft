package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
	yamlv3 "gopkg.in/yaml.v3"
)

// VersionV1 is the only supported document version.
const VersionV1 = "v1"

// Document is the parsed tool-execution policy.
type Document struct {
	Version string
	// Sources declares external tool providers to attach before the
	// chain is built, so their tools are registered by the time scopes
	// and middleware reference them.
	Sources     []SourceEntry
	Middlewares []MiddlewareEntry
	Scopes      map[string]string
}

// MiddlewareEntry is one chain link: a factory kind plus its opaque
// spec. Spec stays raw so each factory decodes exactly the fields it
// owns, strictly.
type MiddlewareEntry struct {
	Kind string
	Spec json.RawMessage
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
func Parse(data []byte) (Document, error) {
	decoder := yamlv3.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var wire documentWire
	if err := decoder.Decode(&wire); err != nil {
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
	doc := Document{
		Version: wire.Version,
		Scopes:  wire.Scopes,
	}
	for _, entry := range wire.Sources {
		doc.Sources = append(doc.Sources, SourceEntry{
			Kind: entry.Kind,
			Spec: entry.Spec.value,
		})
	}
	for _, entry := range wire.Middlewares {
		doc.Middlewares = append(doc.Middlewares, MiddlewareEntry{
			Kind: entry.Kind,
			Spec: entry.Spec.value,
		})
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

type documentWire struct {
	Version     string            `yaml:"version"`
	Sources     []entryWire       `yaml:"sources,omitempty"`
	Middlewares []entryWire       `yaml:"middlewares,omitempty"`
	Scopes      map[string]string `yaml:"scopes,omitempty"`
}

// entryWire is the shared shape of a sources and middlewares entry: a
// kind naming a factory plus an opaque spec that factory owns.
type entryWire struct {
	Kind string  `yaml:"kind"`
	Spec rawSpec `yaml:"spec,omitempty"`
}

// rawSpec captures a middleware spec as JSON, preserving the
// factory's ownership of its own schema.
type rawSpec struct {
	value json.RawMessage
}

func (s *rawSpec) UnmarshalYAML(node *yamlv3.Node) error {
	data, err := yamlNodeJSON(node)
	if err != nil {
		return err
	}
	s.value = data
	return nil
}

// yamlNodeJSON converts a YAML node tree to equivalent JSON. It is a
// trimmed copy of the converter in sdkx/inference/config/yaml,
// covering the scalar/mapping/sequence shapes a middleware spec
// needs.
func yamlNodeJSON(node *yamlv3.Node) (json.RawMessage, error) {
	if node.Kind == yamlv3.DocumentNode {
		if len(node.Content) != 1 {
			return nil, fmt.Errorf("spec YAML document is empty")
		}
		return yamlNodeJSON(node.Content[0])
	}
	switch node.Kind {
	case yamlv3.MappingNode:
		var output bytes.Buffer
		output.WriteByte('{')
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yamlv3.ScalarNode || key.Tag != "!!str" {
				return nil, fmt.Errorf("spec object keys must be strings")
			}
			if index > 0 {
				output.WriteByte(',')
			}
			encodedKey, _ := json.Marshal(key.Value)
			output.Write(encodedKey)
			output.WriteByte(':')
			encodedValue, err := yamlNodeJSON(node.Content[index+1])
			if err != nil {
				return nil, err
			}
			output.Write(encodedValue)
		}
		output.WriteByte('}')
		return output.Bytes(), nil
	case yamlv3.SequenceNode:
		var output bytes.Buffer
		output.WriteByte('[')
		for index, child := range node.Content {
			if index > 0 {
				output.WriteByte(',')
			}
			encoded, err := yamlNodeJSON(child)
			if err != nil {
				return nil, err
			}
			output.Write(encoded)
		}
		output.WriteByte(']')
		return output.Bytes(), nil
	case yamlv3.ScalarNode:
		switch node.Tag {
		case "!!str":
			return json.Marshal(node.Value)
		case "!!bool":
			var value bool
			if err := node.Decode(&value); err != nil {
				return nil, err
			}
			return json.RawMessage(strconv.FormatBool(value)), nil
		case "!!null":
			return json.RawMessage("null"), nil
		case "!!int", "!!float":
			number := strings.ReplaceAll(node.Value, "_", "")
			if !json.Valid([]byte(number)) {
				return nil, fmt.Errorf(
					"spec number %q must use JSON number syntax", node.Value)
			}
			return json.RawMessage(number), nil
		default:
			return nil, fmt.Errorf(
				"spec scalar %q is not JSON-compatible", node.Value)
		}
	case yamlv3.AliasNode:
		return yamlNodeJSON(node.Alias)
	default:
		return nil, fmt.Errorf("unsupported YAML node in spec")
	}
}
