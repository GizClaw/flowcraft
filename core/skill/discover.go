package skill

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/utils/pathsafe"
)

// Diagnostic is a non-fatal discovery diagnostic (a parse failure, a
// tolerated validation fallback, an escaping symlink, ...).
type Diagnostic struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	// Warning marks a tolerated shape issue (a third-party name that
	// differs from its directory, for example) that discovery accepted.
	// Callers log these at a lower severity than real errors.
	Warning bool `json:"warning,omitempty"`
}

// Outcome is the result of one discovery pass.
type Outcome struct {
	// Skills holds every discovered skill, sorted by name and then by
	// path.
	Skills []Metadata
	// Diagnostics holds the non-fatal issues of the pass.
	Diagnostics []Diagnostic
	// Roots lists every configured root that contained at least one
	// skill, in configuration order.
	Roots []string
	// ScanRoots lists every candidate root that was walked, whether or
	// not it contained a skill. Roots are normalized to absolute
	// paths; body reads use the list to keep symlinks from escaping
	// the configured skill roots.
	ScanRoots []string
}

// Discover BFS-walks every root and collects each SKILL.md below it.
// A root may be a single skill directory or a tree of them; a root
// that does not exist is silently empty. Hidden entries are skipped,
// symlinked files and directories are followed only while their
// target stays inside one of the roots, and a skill reachable through
// several roots (or through a symlinked directory) is collected once,
// under its canonical path.
//
// Parse failures are recorded in Diagnostics and never fail the pass.
func Discover(ctx context.Context, roots []string) Outcome {
	if ctx == nil {
		ctx = context.Background()
	}
	scans := absoluteRoots(roots)
	c := collector{
		ctx:       ctx,
		seen:      map[string]bool{},
		foundRoot: map[string]bool{},
		roots:     cleanedRoots(scans),
	}
	for _, root := range scans {
		c.scanRoot(root)
	}
	out := Outcome{
		Skills:      c.skills,
		Diagnostics: c.diagnostics,
		ScanRoots:   scans,
	}
	sort.Slice(out.Skills, func(i, j int) bool {
		if out.Skills[i].Name != out.Skills[j].Name {
			return out.Skills[i].Name < out.Skills[j].Name
		}
		return out.Skills[i].Path < out.Skills[j].Path
	})
	for _, root := range scans {
		if c.foundRoot[filepath.Clean(root)] {
			out.Roots = append(out.Roots, root)
		}
	}
	return out
}

// absoluteRoots normalizes configured roots to absolute cleaned
// paths, dropping empty entries. A root that cannot be made absolute
// (a deleted working directory, say) is kept cleaned as-is.
func absoluteRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			abs = filepath.Clean(root)
		}
		out = append(out, abs)
	}
	return out
}

// cleanedRoots returns the symlink-resolved form of each root, so
// containment checks compare canonical paths (macOS /var is a symlink
// to /private/var, for example).
func cleanedRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		clean := filepath.Clean(root)
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			clean = resolved
		}
		out = append(out, clean)
	}
	return out
}

// insideAnyRoot reports whether resolved stays inside one of roots.
// Callers resolve symlinks first; the comparison is lexical.
func insideAnyRoot(resolved string, roots []string) bool {
	for _, root := range roots {
		if pathsafe.Within(root, resolved) {
			return true
		}
	}
	return false
}

type collector struct {
	ctx         context.Context
	seen        map[string]bool
	foundRoot   map[string]bool
	roots       []string // cleaned scan roots for symlink containment
	skills      []Metadata
	diagnostics []Diagnostic
}

func (c *collector) report(path, message string, warning bool) {
	c.diagnostics = append(c.diagnostics, Diagnostic{
		Path: path, Message: message, Warning: warning,
	})
}

// scanRoot BFS-walks one skill root, following symlinks (verified to
// stay inside the configured roots) and skipping hidden entries, and
// collects every SKILL.md below it. A cancelled context stops the
// walk; the outcome stays partial.
func (c *collector) scanRoot(root string) {
	queue := []string{root}
	for len(queue) > 0 {
		if c.ctx.Err() != nil {
			return
		}
		dir := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				c.report(dir, err.Error(), false)
			}
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			full := filepath.Join(dir, name)
			if entry.Type()&fs.ModeSymlink != 0 {
				// Resolve once and verify the target stays inside the
				// configured skill roots: a repo-supplied symlink must
				// never redirect discovery (or a body read) outside
				// them.
				target, err := filepath.EvalSymlinks(full)
				if err != nil {
					c.report(full, "resolve symlink: "+err.Error(), false)
					continue
				}
				if !insideAnyRoot(target, c.roots) {
					c.report(full, "symlink escapes the configured skill roots", false)
					continue
				}
			}
			info, err := os.Stat(full)
			if err != nil {
				continue
			}
			if info.IsDir() {
				queue = append(queue, full)
				continue
			}
			if name != "SKILL.md" {
				continue
			}
			// Canonicalize the file so a skill reachable through
			// several roots (or through a symlinked directory) is
			// collected once, under the path the readers resolve to.
			canonical, err := filepath.EvalSymlinks(full)
			if err != nil {
				c.report(full, "resolve symlink: "+err.Error(), false)
				continue
			}
			if !insideAnyRoot(canonical, c.roots) {
				c.report(full, "symlink escapes the configured skill roots", false)
				continue
			}
			if c.seen[canonical] {
				continue
			}
			res, err := ParseFile(canonical)
			if err != nil {
				c.report(full, err.Error(), false)
				continue
			}
			for _, w := range res.Warnings {
				c.report(full, w, true)
			}
			c.seen[canonical] = true
			c.skills = append(c.skills, res.Metadata)
			c.foundRoot[filepath.Clean(root)] = true
		}
	}
}
