package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/craft/internal/version"
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
		!version.Valid(m.Version) {
		return errdefs.Validationf(
			"plugin %s: invalid version %q", m.ID, m.Version)
	}
	if m.MinHostVersion != "" && !version.Valid(m.MinHostVersion) {
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
	// Two servers whose tools would share a namespace shadow each other:
	// the registry keeps the first name it is given and the other tool
	// silently does not exist. The namespace of one server is
	// ToolNamespaces' answer, so that is what has to be distinct.
	seenNamespace := make(map[string]int, len(servers))
	for i, namespace := range ToolNamespaces(m.ID, servers) {
		if first, duplicate := seenNamespace[namespace]; duplicate {
			return errdefs.Validationf(
				"plugin %s: mcp servers %d and %d share the tool namespace %q; "+
					"name them differently", m.ID, first, i, namespace)
		}
		seenNamespace[namespace] = i
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
			return errdefs.Validationf("plugin %s: update.url %w", m.ID, err)
		}
	}
	if root == "" {
		return nil
	}
	// The stdio command is checked here, where the root is already
	// known: "the file is not there" belongs to install, not to a
	// background spawn that would otherwise retry it in silence.
	for i, server := range servers {
		if err := server.validatePaths(m.ID, i, root); err != nil {
			return err
		}
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

// effectiveTransport is the transport after the defaulting rules that
// validate and buildTransport share: an explicit transport wins, then a
// url means http, anything else is stdio.
func (s MCPServer) effectiveTransport() string {
	transport := strings.TrimSpace(s.Transport)
	if transport == "" {
		if s.URL != "" {
			return "http"
		}
		return "stdio"
	}
	return transport
}

func (s MCPServer) validate(id string, index int) error {
	transport := s.effectiveTransport()
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

// validatePaths checks the stdio command and its path-bearing arguments
// against the plugin root. It mirrors [ResolveCommand] — the same
// separator test, the same skip for flags — so that a manifest which
// installs is one the plugin host can actually spawn: install-time
// checks and spawn-time resolution cannot disagree about which files
// must exist. Bare names stay unchecked: a bare command is a PATH
// lookup, and a bare argument is not a path at all. The command must
// not be a directory, because exec cannot run one; an argument only has
// to exist, because an argument may name a directory on purpose
// ("go run ./server" takes a package path).
func (s MCPServer) validatePaths(id string, index int, root string) error {
	if s.effectiveTransport() != "stdio" {
		return nil
	}
	command := strings.TrimSpace(s.Command)
	if carriesSeparator(command) {
		if err := checkExecPath(
			id, fmt.Sprintf("mcp server %d command", index), command, root,
		); err != nil {
			return err
		}
	}
	for _, arg := range s.Args {
		if arg == "" || strings.HasPrefix(arg, "-") {
			// ResolveCommand leaves flags alone — the value after the
			// "=" may be a path that is not ours to resolve — so the
			// check must too.
			continue
		}
		if !carriesSeparator(arg) {
			continue
		}
		// Only existence: what an argument means is the child's
		// business, and a directory is a legitimate package path.
		if _, err := checkPath(
			id, fmt.Sprintf("mcp server %d arg", index), arg, root,
		); err != nil {
			return err
		}
	}
	return nil
}

// carriesSeparator reports whether a command or argument names a path
// rather than a bare word; it is the test ResolveCommand applies.
func carriesSeparator(value string) bool {
	return strings.ContainsRune(value, '/') ||
		strings.ContainsRune(value, filepath.Separator)
}

func checkRelativePath(id, field string, index int, rel, root string) error {
	if _, err := checkPath(id, field, rel, root); err != nil {
		return errdefs.Validationf("%w (index %d)", err, index)
	}
	return nil
}

func checkPath(id, field, rel, root string) (string, error) {
	path, info, err := statPluginPath(id, field, rel, root)
	if err != nil {
		return "", err
	}
	if info.IsDir() && field == "ui.entry" {
		return "", errdefs.Validationf(
			"plugin %s: ui.entry %q is a directory", id, rel)
	}
	return path, nil
}

// checkExecPath is the executable-shaped variant of checkPath: the path
// must exist and must be a regular file, because exec cannot run
// anything else (os.Stat follows symlinks, so a link to a script counts).
func checkExecPath(id, field, rel, root string) error {
	_, info, err := statPluginPath(id, field, rel, root)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		what := "is not a regular file"
		if info.IsDir() {
			what = "is a directory"
		}
		return errdefs.Validationf(
			"plugin %s: %s %q %s", id, field, rel, what)
	}
	return nil
}

// statPluginPath resolves rel inside root and stats it.
func statPluginPath(id, field, rel, root string) (string, os.FileInfo, error) {
	if len(rel) > maxPathLen {
		return "", nil, errdefs.Validationf(
			"plugin %s: %s path exceeds %d characters", id, field, maxPathLen)
	}
	path, err := ResolvePath(root, rel)
	if err != nil {
		return "", nil, errdefs.Validationf("plugin %s: %s: %w", id, field, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, errdefs.Validationf(
			"plugin %s: %s %q: %w", id, field, rel, err)
	}
	return filepath.Clean(path), info, nil
}
