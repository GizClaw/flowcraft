// Package skills embeds built-in SKILL.md files for preinstallation.
package skills

import (
	"embed"
	"io/fs"
)

//go:embed weather/SKILL.md github/SKILL.md summarize/SKILL.md coding-agent/SKILL.md
var builtinFS embed.FS

// BuiltinFS returns the embedded filesystem containing built-in skills.
// Each skill is in a subdirectory named after the skill, containing SKILL.md.
func BuiltinFS() fs.FS {
	return builtinFS
}
