package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/config/utils"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// VersionV1 is the only supported document version.
const VersionV1 = "v1"

// Document is the parsed tool-execution policy.
type Document struct {
	Version string `json:"version"`
	// Sources declares external tool providers to attach before the
	// chain is built, so their tools are registered by the time scopes
	// and middleware reference them.
	Sources     []SourceEntry     `json:"sources,omitempty"`
	Middlewares []MiddlewareEntry `json:"middlewares,omitempty"`
	Scopes      map[string]string `json:"scopes,omitempty"`
}

// MiddlewareEntry is one chain link: a factory kind plus its opaque
// spec. Spec stays an undecoded JSON subtree so each factory decodes
// exactly the fields it owns, strictly.
type MiddlewareEntry struct {
	Kind string          `json:"kind"`
	Spec json.RawMessage `json:"spec,omitempty"`
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

// Parse strictly decodes and validates one document. YAML and JSON are
// both accepted; unknown fields, trailing documents, and invalid values
// are all errors. Specs are carried as opaque JSON subtrees and decoded
// by their owning factory.
func Parse(data []byte) (Document, error) {
	doc, err := utils.Decode[Document](data)
	if err != nil {
		return Document{}, errdefs.Validation(fmt.Errorf(
			"decode tool config: %w", err))
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// DecodeSpec strictly decodes a factory's spec subtree into T: unknown
// keys are errors, so a typo in configuration fails the build instead
// of silently dropping policy. Factories SHOULD decode through this
// helper.
//
// A nil or empty spec is an error: a factory calling DecodeSpec has
// fields it needs. Kinds that take no spec reject one explicitly
// instead.
func DecodeSpec[T any](spec json.RawMessage) (T, error) {
	var out T
	if len(spec) == 0 {
		return out, errdefs.Validation(fmt.Errorf("spec is required"))
	}
	var err error
	out, err = sdkconfig.DecodeSettings[T](spec)
	if err != nil {
		return out, errdefs.Validation(fmt.Errorf("invalid spec: %w", err))
	}
	return out, nil
}

// isEmptySpec reports whether a spec carries no content, which is how a
// "takes no spec" kind distinguishes an absent spec from one an author
// wrote by mistake.
func isEmptySpec(spec json.RawMessage) bool {
	if len(spec) == 0 {
		return true
	}
	trimmed := bytes.TrimSpace(spec)
	return bytes.Equal(trimmed, []byte("{}")) ||
		bytes.Equal(trimmed, []byte("[]")) ||
		bytes.Equal(trimmed, []byte("null"))
}
