package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Manifest limits (shared with the design doc).
const (
	MaxManifestBytes = 1 << 20

	maxSkills  = 32
	maxHooks   = 16
	maxServers = 16
	maxNodes   = 64
	maxPathLen = 256
	maxEnvKeys = 32
	maxEnvKey  = 128
	maxEnvVal  = 4096
)

var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*(-[0-9A-Za-z.-]+)?$`)

// Permissions accepted in a manifest.
var validPermissions = map[string]struct{}{
	"mcp:provide":      {},
	"skills:provide":   {},
	"hooks:provide":    {},
	"nodes:provide":    {},
	"ui:webview":       {},
	"storage:kv":       {},
	"secrets:auth":     {},
	"inference:write":  {},
	"telemetry:export": {},
	"sessions:import":  {},
	"host:open_url":    {},
	"events:emit":      {},
}

// Manifest is the decoded plugin.json.
type Manifest struct {
	ID             string      `json:"id"`
	Name           string      `json:"name,omitempty"`
	Version        string      `json:"version"`
	MinHostVersion string      `json:"minHostVersion,omitempty"`
	Permissions    []string    `json:"permissions,omitempty"`
	UI             *UIManifest `json:"ui,omitempty"`
	MCP            *MCPServer  `json:"mcp,omitempty"`
	Update         *UpdateDecl `json:"update,omitempty"`
	Skills         []string    `json:"skills,omitempty"`
	Hooks          []string    `json:"hooks,omitempty"`
	Nodes          []NodeDecl  `json:"nodes,omitempty"`

	// Legacy aliases accepted from opencraft manifests.
	LegacyEntry      string      `json:"entry,omitempty"`
	LegacyMCPServers []MCPServer `json:"mcpServers,omitempty"`

	// Legacy features rejected with an upgrade hint.
	Kraft json.RawMessage `json:"kraft,omitempty"`
	Tools json.RawMessage `json:"tools,omitempty"`
}

// UIManifest points at the plugin's UI bundle.
type UIManifest struct {
	Entry string `json:"entry"`
}

// UpdateDecl declares where an application can look for a newer version
// of the plugin: a JSON manifest at url (see UpdateInfo). craft checks
// the shape and hands the URL to an Installer; it never fetches
// anything itself.
type UpdateDecl struct {
	URL string `json:"url"`
}

// MCPServer declares one MCP server the plugin ships.
type MCPServer struct {
	Name      string            `json:"name,omitempty"`
	Transport string            `json:"transport,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Required  bool              `json:"required,omitempty"`
}

// NodeDecl declares one plugin-provided graph node.
type NodeDecl struct {
	Type    string `json:"type"`
	Tool    string `json:"tool"`
	Desc    string `json:"desc,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// Servers returns the effective server list (new field or legacy alias).
func (m Manifest) Servers() []MCPServer {
	if m.MCP != nil {
		return []MCPServer{*m.MCP}
	}
	return m.LegacyMCPServers
}

// Entry returns the effective UI entry (new field or legacy alias).
func (m Manifest) Entry() string {
	if m.UI != nil && m.UI.Entry != "" {
		return m.UI.Entry
	}
	return m.LegacyEntry
}

// HasPermission reports whether the manifest declares permission.
func (m Manifest) HasPermission(permission string) bool {
	for _, candidate := range m.Permissions {
		if candidate == permission {
			return true
		}
	}
	return false
}

// ParseManifest decodes and validates one plugin.json.
func ParseManifest(ctx context.Context, data []byte) (Manifest, error) {
	if len(data) > MaxManifestBytes {
		return Manifest{}, errdefs.Validationf(
			"plugin: manifest exceeds %d bytes", MaxManifestBytes)
	}
	var manifest Manifest
	if err := resource.DecodeSettings(ctx, &manifest, data); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate checks the manifest against its plugin root. Root may be
// empty to skip filesystem checks.
func (m Manifest) Validate(root string) error {
	if err := ValidateID(m.ID); err != nil {
		return err
	}
	if strings.TrimSpace(m.Version) == "" ||
		!versionPattern.MatchString(m.Version) {
		return errdefs.Validationf(
			"plugin %s: invalid version %q", m.ID, m.Version)
	}
	if m.MinHostVersion != "" && !versionPattern.MatchString(m.MinHostVersion) {
		return errdefs.Validationf(
			"plugin %s: invalid minHostVersion %q", m.ID, m.MinHostVersion)
	}
	if len(m.Kraft) > 0 {
		return errdefs.Validationf(
			"plugin %s: the kraft field is no longer supported; "+
				"expose plugin tools through an MCP server", m.ID)
	}
	if len(m.Tools) > 0 {
		return errdefs.Validationf(
			"plugin %s: the tools field is no longer supported; "+
				"declare them as MCP tools in the plugin server", m.ID)
	}
	if m.MCP != nil && len(m.LegacyMCPServers) > 0 {
		return errdefs.Validationf(
			"plugin %s: mcp and mcpServers are mutually exclusive", m.ID)
	}
	for _, permission := range m.Permissions {
		if _, ok := validPermissions[permission]; !ok {
			return errdefs.Validationf(
				"plugin %s: unknown permission %q", m.ID, permission)
		}
	}
	if len(m.Skills) > maxSkills {
		return errdefs.Validationf(
			"plugin %s: at most %d skills", m.ID, maxSkills)
	}
	if len(m.Hooks) > maxHooks {
		return errdefs.Validationf(
			"plugin %s: at most %d hooks", m.ID, maxHooks)
	}
	servers := m.Servers()
	if len(servers) > maxServers {
		return errdefs.Validationf(
			"plugin %s: at most %d MCP servers", m.ID, maxServers)
	}
	if len(m.Nodes) > maxNodes {
		return errdefs.Validationf(
			"plugin %s: at most %d graph nodes", m.ID, maxNodes)
	}
	for i, server := range servers {
		if err := server.validate(m.ID, i); err != nil {
			return err
		}
	}
	for i, node := range m.Nodes {
		if strings.TrimSpace(node.Type) == "" ||
			strings.TrimSpace(node.Tool) == "" {
			return errdefs.Validationf(
				"plugin %s: nodes[%d] requires type and tool", m.ID, i)
		}
	}
	// The update endpoint is checked before the filesystem half: it
	// says nothing about the plugin root, so it is an error whether or
	// not a root was handed in.
	if m.Update != nil {
		if err := checkRemoteURL(m.Update.URL); err != nil {
			return errdefs.Validationf("plugin %s: update.url %v", m.ID, err)
		}
	}
	if root == "" {
		return nil
	}
	for i, rel := range m.Skills {
		if err := checkRelativePath(m.ID, "skills", i, rel, root); err != nil {
			return err
		}
	}
	for i, rel := range m.Hooks {
		if err := checkRelativePath(m.ID, "hooks", i, rel, root); err != nil {
			return err
		}
	}
	if entry := m.Entry(); entry != "" {
		if _, err := checkPath(m.ID, "ui.entry", entry, root); err != nil {
			return err
		}
	}
	return nil
}

func (s MCPServer) validate(id string, index int) error {
	transport := strings.TrimSpace(s.Transport)
	if transport == "" {
		if s.URL != "" {
			transport = "http"
		} else {
			transport = "stdio"
		}
	}
	switch transport {
	case "stdio":
		if strings.TrimSpace(s.Command) == "" {
			return errdefs.Validationf(
				"plugin %s: mcp server %d requires command", id, index)
		}
	case "http":
		if strings.TrimSpace(s.URL) == "" {
			return errdefs.Validationf(
				"plugin %s: mcp server %d requires url", id, index)
		}
	default:
		return errdefs.Validationf(
			"plugin %s: mcp server %d has unknown transport %q",
			id, index, transport)
	}
	if len(s.Env) > maxEnvKeys {
		return errdefs.Validationf(
			"plugin %s: mcp server %d has too many env entries", id, index)
	}
	for key, value := range s.Env {
		if len(key) > maxEnvKey || len(value) > maxEnvVal {
			return errdefs.Validationf(
				"plugin %s: mcp server %d env %q exceeds bounds", id, index, key)
		}
	}
	return nil
}

func checkRelativePath(id, field string, index int, rel, root string) error {
	if _, err := checkPath(id, field, rel, root); err != nil {
		return errdefs.Validationf("%v (index %d)", err, index)
	}
	return nil
}

func checkPath(id, field, rel, root string) (string, error) {
	if len(rel) > maxPathLen {
		return "", errdefs.Validationf(
			"plugin %s: %s path exceeds %d characters", id, field, maxPathLen)
	}
	path, err := ResolvePath(root, rel)
	if err != nil {
		return "", errdefs.Validationf("plugin %s: %s: %v", id, field, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", errdefs.Validationf(
			"plugin %s: %s %q: %v", id, field, rel, err)
	}
	if info.IsDir() && field == "ui.entry" {
		return "", errdefs.Validationf(
			"plugin %s: ui.entry %q is a directory", id, rel)
	}
	return filepath.Clean(path), nil
}
