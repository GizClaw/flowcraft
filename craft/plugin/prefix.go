package plugin

import (
	"path/filepath"
	"regexp"
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
