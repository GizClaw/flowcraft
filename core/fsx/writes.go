package fsx

import (
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/permission"
)

// cleanWrites normalizes the declared paths so two spellings of one directory
// collapse before validation catches the duplicate.
func cleanWrites(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		out = append(out, filepath.Clean(path))
	}
	return out
}

// validateWrites delegates to the permission rule: absolute, glob-free, unique.
// The same rule the sandbox projection relies on, so a path either package
// accepts is a path the other can mount.
func validateWrites(paths []string) error {
	return permission.Writes{Paths: paths}.Validate()
}
