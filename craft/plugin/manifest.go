package plugin

import (
	"context"
	"encoding/json"
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
	maxNodes   = 64
	maxPathLen = 256
	maxEnvKeys = 32
	maxEnvKey  = 128
	maxEnvVal  = 4096
)

// Permissions craft itself defines: the contribution permissions its
// charter pairs with a gate and a drop rule. Grants for host primitives are
// not in this table, because craft defines no primitives — the application
// declares those names (see [Options.Grants]) and [Manifest.Validate]
// accepts them alongside these.
var validPermissions = map[string]struct{}{
	"tools:provide":  {},
	"skills:provide": {},
	"hooks:provide":  {},
	"nodes:provide":  {},
	"ui:webview":     {},
	"storage:kv":     {},
}

// Manifest is the decoded plugin.json.
type Manifest struct {
	ID             string      `json:"id"`
	Name           string      `json:"name,omitempty"`
	Version        string      `json:"version"`
	MinHostVersion string      `json:"minHostVersion,omitempty"`
	Permissions    []string    `json:"permissions,omitempty"`
	Run            *RunSpec    `json:"run,omitempty"`
	UI             *UIManifest `json:"ui,omitempty"`
	Update         *UpdateDecl `json:"update,omitempty"`
	Skills         []string    `json:"skills,omitempty"`
	Hooks          []string    `json:"hooks,omitempty"`
	Nodes          []NodeDecl  `json:"nodes,omitempty"`

	// Legacy features rejected with an upgrade hint.
	Kraft json.RawMessage `json:"kraft,omitempty"`
	Tools json.RawMessage `json:"tools,omitempty"`
}

// RunSpec is how craft starts the plugin process. There is no transport
// field: craft serves the only socket, the plugin dials what
// FLOWCRAFT_PLUGIN_WIRE names, and the connection is the plugin's identity.
type RunSpec struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
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

// NodeDecl declares one plugin-provided graph node.
type NodeDecl struct {
	Type    string `json:"type"`
	Tool    string `json:"tool"`
	Desc    string `json:"desc,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// Entry returns the UI entry point.
func (m Manifest) Entry() string {
	if m.UI == nil {
		return ""
	}
	return m.UI.Entry
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
	if err := refuseLegacyFields(data); err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := resource.DecodeSettings(ctx, &manifest, data); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// refuseLegacyFields names the fields the wire redesign removed. The
// decoder would reject them anyway — it disallows unknown fields — but it
// would do so with "json: unknown field \"mcp\"", which tells an operator
// nothing about what replaced it. A plugin that still declares an MCP server
// would otherwise install, run, publish nothing, and leave no hint as to
// why.
func refuseLegacyFields(data []byte) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		// Not an object: the typed decode below reports that far better
		// than a guess here would.
		return nil
	}
	for _, field := range []string{"mcp", "mcpServers"} {
		if raw, ok := document[field]; !ok || len(raw) == 0 {
			continue
		}
		return errdefs.Validationf(
			"plugin: the %s field is no longer supported; craft serves the "+
				"plugin socket and the plugin dials FLOWCRAFT_PLUGIN_WIRE. "+
				"Declare a run section and speak the plugin wire", field)
	}
	return nil
}

// Validate checks the manifest against its plugin root. Root may be
// empty to skip filesystem checks.
//
// grants are the application's grant names — the ones the host primitives
// it serves require — and are accepted on top of craft's own permissions.
// Craft defines no primitive grants itself, so this is the only way a
// manifest can ask for one.
func (m Manifest) Validate(root string, grants ...string) error {
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
				"declare tools through the plugin wire instead", m.ID)
	}
	if len(m.Tools) > 0 {
		return errdefs.Validationf(
			"plugin %s: the tools field is no longer supported; "+
				"declare them through the plugin wire instead", m.ID)
	}
	known := make(map[string]bool, len(validPermissions)+len(grants))
	for permission := range validPermissions {
		known[permission] = true
	}
	for _, grant := range grants {
		known[grant] = true
	}
	for _, permission := range m.Permissions {
		if !known[permission] {
			return errdefs.Validationf(
				"plugin %s: unknown permission %q", m.ID, permission)
		}
	}
	if err := m.checkRunGrants(); err != nil {
		return err
	}
	if m.Run != nil {
		if err := m.Run.validate(m.ID); err != nil {
			return err
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
	if len(m.Nodes) > maxNodes {
		return errdefs.Validationf(
			"plugin %s: at most %d graph nodes", m.ID, maxNodes)
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
	// The command is checked here, where the root is already known: "the
	// file is not there" belongs to install, not to a background spawn
	// that would otherwise retry it in silence.
	if m.Run != nil {
		if err := m.Run.validatePaths(m.ID, root); err != nil {
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

func (s RunSpec) validate(id string) error {
	if strings.TrimSpace(s.Command) == "" {
		return errdefs.Validationf(
			"plugin %s: run.command is required", id)
	}
	if len(s.Env) > maxEnvKeys {
		return errdefs.Validationf(
			"plugin %s: run has too many env entries", id)
	}
	for key, value := range s.Env {
		if len(key) > maxEnvKey || len(value) > maxEnvVal {
			return errdefs.Validationf(
				"plugin %s: run env %q exceeds bounds", id, key)
		}
	}
	return nil
}

// validatePaths checks the run command and its path-bearing arguments
// against the plugin root. It mirrors [ResolveCommand] — the same separator
// test, the same skip for flags — so that a manifest which installs is one
// the plugin host can actually spawn: install-time checks and spawn-time
// resolution cannot disagree about which files must exist.
//
// Bare names stay unchecked: a bare command is a PATH lookup, and a bare
// argument is not a path at all. The command must not be a directory,
// because exec cannot run one; an argument only has to exist, because an
// argument may name a directory on purpose ("go run ./server" takes a
// package path).
func (s RunSpec) validatePaths(id, root string) error {
	command := strings.TrimSpace(s.Command)
	if carriesSeparator(command) {
		if err := checkExecPath(id, "run.command", command, root); err != nil {
			return err
		}
	}
	for _, arg := range s.Args {
		if arg == "" || strings.HasPrefix(arg, "-") {
			// ResolveCommand leaves flags alone — the value after the "="
			// may be a path that is not ours to resolve — so the check
			// must too.
			continue
		}
		if !carriesSeparator(arg) {
			continue
		}
		// Only existence: what an argument means is the child's business,
		// and a directory is a legitimate package path.
		if _, err := checkPath(id, "run.arg", arg, root); err != nil {
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
// must exist and must be a regular file, because exec cannot run anything
// else (os.Stat follows symlinks, so a link to a script counts).
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
