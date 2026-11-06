package skill

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/fsx"
)

// SkillFile is the document every skill directory holds.
const SkillFile = "SKILL.md"

// Diagnostic is a non-fatal discovery diagnostic: a parse failure, a tolerated
// shape issue, or a read error.
type Diagnostic struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	// Warning marks a tolerated shape issue that discovery accepted.
	Warning bool `json:"warning,omitempty"`
}

// Outcome is one discovery pass.
type Outcome struct {
	// Skills holds every discovered skill, sorted by name then path.
	Skills []Metadata `json:"skills"`
	// Diagnostics holds the non-fatal issues of the pass.
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	// Roots lists the configured roots that contained at least one skill.
	Roots []string `json:"roots,omitempty"`
}

// Discover walks every root through tree and parses each SKILL.md below it.
//
// Reading goes through fsx, so what a skill tree may contain is the caller's
// policy, not this package's. Hidden entries are skipped: .git and friends are
// never skills. A root that cannot be listed is silently empty — a deployment
// names conventional roots that may not exist on every machine.
//
// Parse failures and read errors land in Diagnostics; they never fail the pass,
// because one broken third-party skill must not hide the rest.
func Discover(ctx context.Context, tree *fsx.Tree, roots []string) Outcome {
	out := Outcome{}
	seenDir := make(map[string]bool, len(roots))
	seenSkill := make(map[string]bool)

	for _, root := range roots {
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			abs = filepath.Clean(root)
		}
		if seenDir[abs] {
			continue
		}
		seenDir[abs] = true
		if walkSkills(ctx, tree, abs, &out, seenSkill) {
			out.Roots = append(out.Roots, abs)
		}
	}

	sort.Slice(out.Skills, func(i, j int) bool {
		if out.Skills[i].Name != out.Skills[j].Name {
			return out.Skills[i].Name < out.Skills[j].Name
		}
		return out.Skills[i].Path < out.Skills[j].Path
	})
	return out
}

// walkSkills visits dir recursively and reports whether it held a skill.
func walkSkills(
	ctx context.Context,
	tree *fsx.Tree,
	dir string,
	out *Outcome,
	seenSkill map[string]bool,
) bool {
	entries, err := tree.List(ctx, dir)
	if err != nil {
		// A missing root is the normal case for a conventional path; a real
		// read error is already reported by Read/List to the caller in other
		// paths, and failing discovery over it would hide the rest.
		return false
	}

	found := false
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		if entry.IsDir() {
			if walkSkills(ctx, tree, full, out, seenSkill) {
				found = true
			}
			continue
		}
		if name != SkillFile {
			continue
		}

		data, err := tree.Read(ctx, full)
		if err != nil {
			out.Diagnostics = append(out.Diagnostics, Diagnostic{Path: full, Message: err.Error()})
			continue
		}
		result, err := Parse(full, data)
		if err != nil {
			out.Diagnostics = append(out.Diagnostics, Diagnostic{Path: full, Message: err.Error()})
			continue
		}
		for _, warning := range result.Warnings {
			out.Diagnostics = append(out.Diagnostics, Diagnostic{
				Path: full, Message: warning, Warning: true,
			})
		}
		if seenSkill[result.Metadata.Path] {
			continue
		}
		seenSkill[result.Metadata.Path] = true
		out.Skills = append(out.Skills, result.Metadata)
		found = true
	}
	return found
}
