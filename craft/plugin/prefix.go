package plugin

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidateID checks the plugin id spelling.
func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return errdefs.Validationf("plugin: invalid id %q", id)
	}
	return nil
}

// ToolPrefix is the tool-name namespace of a plugin: [a-z0-9_-] only,
// suffixed with the MCP prefix separator.
func ToolPrefix(id string) string {
	var builder strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String() + "__"
}

// ToolNamespaces returns the tool-name namespace of every MCP server of
// one plugin, in declaration order. A single-server plugin keeps the
// plugin namespace, ToolPrefix(id). A plugin with several servers adds
// the server's name — or its position, when the manifest does not name
// it — so two servers of one manifest cannot publish the same tool name:
// the registry keeps the first registration and the second server's tool
// would simply not exist. Manifest.Validate rejects the manifests where
// even that is not enough (two servers with the same name).
func ToolNamespaces(id string, servers []MCPServer) []string {
	namespaces := make([]string, 0, len(servers))
	for index, server := range servers {
		namespace := ToolPrefix(id)
		if len(servers) > 1 {
			tag := strings.TrimSpace(server.Name)
			if tag == "" {
				tag = strconv.Itoa(index + 1)
			}
			namespace += ToolPrefix(tag)
		}
		namespaces = append(namespaces, namespace)
	}
	return namespaces
}

// ResolvePath confines rel inside root and returns the absolute path.
func ResolvePath(root, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errdefs.Validationf("plugin: empty path")
	}
	if filepath.IsAbs(rel) {
		return "", errdefs.Validationf(
			"plugin: path %q must be relative", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errdefs.Forbiddenf(
			"plugin: path %q escapes the plugin root", rel)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", errdefs.Validationf("plugin: resolve root: %v", err)
	}
	joined := filepath.Join(absoluteRoot, clean)
	relative, err := filepath.Rel(absoluteRoot, joined)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errdefs.Forbiddenf(
			"plugin: path %q escapes the plugin root", rel)
	}
	return joined, nil
}

// ResolveCommand resolves a plugin-relative command and its arguments:
// every path that contains a separator stays inside the plugin root,
// bare names are looked up through PATH by the OS.
//
// Arguments carrying a path are resolved whether or not the command
// itself has one, because the two are independent: the documented
// Python shape is a bare interpreter with a plugin-relative script
// ("command": "python3", "args": ["server/main.py"]), and the child's
// working directory is not the plugin root.
func ResolveCommand(root, command string, args []string) (string, []string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", nil, errdefs.Validationf("plugin: mcp.command is required")
	}
	resolvedArgs := append([]string(nil), args...)
	for i, arg := range resolvedArgs {
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		if strings.ContainsRune(arg, filepath.Separator) ||
			strings.ContainsRune(arg, '/') {
			resolved, err := ResolvePath(root, arg)
			if err != nil {
				return "", nil, err
			}
			resolvedArgs[i] = resolved
		}
	}
	if !strings.ContainsRune(command, filepath.Separator) &&
		!strings.ContainsRune(command, '/') {
		return command, resolvedArgs, nil
	}
	path, err := ResolvePath(root, command)
	if err != nil {
		return "", nil, err
	}
	return path, resolvedArgs, nil
}
