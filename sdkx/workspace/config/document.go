package config

import (
	"bytes"
	"fmt"
	"io"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	yamlv3 "gopkg.in/yaml.v3"
)

// VersionV1 is the only supported document version.
const VersionV1 = "v1"

// Document declares named workspace resources.
type Document struct {
	Version    string                    `yaml:"version"`
	Workspaces map[string]WorkspaceEntry `yaml:"workspaces"`
}

// WorkspaceEntry selects a driver and carries its driver-owned settings.
type WorkspaceEntry struct {
	Driver   string  `yaml:"driver"`
	Settings *Opaque `yaml:"settings,omitempty"`
	Scope    *Scope  `yaml:"scope,omitempty"`
}

// Scope configures the policy applied by workspace.NewScopedWorkspace.
type Scope struct {
	DenyRead      []string `yaml:"deny_read,omitempty"`
	AllowWrite    []string `yaml:"allow_write,omitempty"`
	MandatoryDeny []string `yaml:"mandatory_deny,omitempty"`
}

// Opaque captures a YAML subtree for strict decoding by its driver.
type Opaque yamlv3.Node

// UnmarshalYAML stores the subtree without applying document field rules to it.
func (o *Opaque) UnmarshalYAML(node *yamlv3.Node) error {
	*o = Opaque(*node)
	return nil
}

// Node returns the captured settings subtree.
func (o *Opaque) Node() *yamlv3.Node {
	if o == nil {
		return nil
	}
	return (*yamlv3.Node)(o)
}

// Validate checks invariants owned by the versioned document.
func (d Document) Validate() error {
	if d.Version != VersionV1 {
		return errdefs.Validationf(
			"workspace config version %q is not supported (want %q)",
			d.Version, VersionV1)
	}
	if d.Workspaces == nil {
		return errdefs.Validationf("workspace config workspaces map is required")
	}
	for name, entry := range d.Workspaces {
		if name == "" {
			return errdefs.Validationf("workspace config workspaces: empty workspace name")
		}
		if entry.Driver == "" {
			return errdefs.Validationf(
				"workspace config workspaces[%q]: driver is required", name)
		}
		if entry.Scope != nil {
			if err := entry.Scope.validate(); err != nil {
				return errdefs.Validationf(
					"workspace config workspaces[%q].scope: %v", name, err)
			}
		}
	}
	return nil
}

func (s Scope) validate() error {
	groups := []struct {
		name     string
		patterns []string
	}{
		{"deny_read", s.DenyRead},
		{"allow_write", s.AllowWrite},
		{"mandatory_deny", s.MandatoryDeny},
	}
	for _, group := range groups {
		for i, pattern := range group.patterns {
			if pattern == "" {
				return fmt.Errorf("%s[%d]: pattern is empty", group.name, i)
			}
			if filepath.IsAbs(pattern) || pattern == ".." ||
				strings.HasPrefix(pattern, "../") {
				return fmt.Errorf("%s[%d]: pattern %q must be relative", group.name, i, pattern)
			}
			if _, err := pathpkg.Match(filepath.ToSlash(pattern), "validate"); err != nil {
				return fmt.Errorf("%s[%d]: invalid pattern %q: %w",
					group.name, i, pattern, err)
			}
		}
	}
	return nil
}

// Parse strictly decodes and validates one YAML document.
func Parse(data []byte) (Document, error) {
	decoder := yamlv3.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, errdefs.Validationf(
			"decode workspace config YAML: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents")
		}
		return Document{}, errdefs.Validationf(
			"decode workspace config YAML: %v", err)
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// DecodeSettings strictly decodes an opaque settings node into T.
// A nil node produces T's zero value.
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
