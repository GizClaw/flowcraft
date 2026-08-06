package config

import (
	"fmt"
	pathpkg "path"
	"path/filepath"
	"strings"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/config/utils"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// VersionV1 is the only supported document version.
const VersionV1 = "v1"

// Document declares named workspace resources.
type Document struct {
	Version    string                    `json:"version"`
	Workspaces map[string]WorkspaceEntry `json:"workspaces"`
}

// WorkspaceEntry selects a driver and carries its driver-owned settings.
type WorkspaceEntry struct {
	Driver   string            `json:"driver"`
	Settings *sdkconfig.Opaque `json:"settings,omitempty"`
	Scope    *Scope            `json:"scope,omitempty"`
}

// Scope configures the policy applied by workspace.NewScopedWorkspace.
type Scope struct {
	DenyRead      []string `json:"deny_read,omitempty"`
	AllowWrite    []string `json:"allow_write,omitempty"`
	MandatoryDeny []string `json:"mandatory_deny,omitempty"`
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

// Parse strictly decodes and validates one document. YAML and JSON are
// both accepted; unknown fields and trailing documents are errors.
func Parse(data []byte) (Document, error) {
	doc, err := utils.Decode[Document](data)
	if err != nil {
		return Document{}, errdefs.Validationf(
			"decode workspace config: %v", err)
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}
