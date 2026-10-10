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
	// skill, in configuration order and de-duplicated: a root that
	// resolves to an already-walked directory is listed once, the
	// first time it is reached.
	Roots []string
	// ScanRoots lists every configured root in absolute form, whether
	// or not it contained a skill and whether or not the walk had to
	// cover it again. Body reads use the list to keep symlinks from
	// escaping the configured skill roots.
	ScanRoots []string
}

// Discover BFS-walks every root and collects each SKILL.md below it.
// A root may be a single skill directory or a tree of them; a root
// that does not exist is silently empty. Hidden entries are skipped,
// symlinked files and directories are followed only while their
// target stays inside one of the roots, and a skill reachable through
// several roots (or through a symlinked directory) is collected once,
// under its canonical path. Directories are walked at most once,
// tracked by canonical path, so an alias that points back into a
// walked tree cannot loop or re-descend: discovery terminates on any
// tree, however it is linked.
//
// Parse failures are recorded in Diagnostics and never fail the pass.
func Discover(ctx context.Context, roots []string) Outcome {
	if ctx == nil {
		ctx = context.Background()
	}
	scans := absoluteRoots(roots)
	c := collector{
		ctx:      ctx,
		seen:     map[string]bool{},
		seenDirs: map[string]bool{},
		roots:    cleanedRoots(scans),
	}
	var found []string
	for _, root := range scans {
		// Walk the canonical form of a root, and walk a directory at
		// most once: a root configured twice — or a second root that
		// resolves to a directory the walk already covered — is not
		// walked or listed again.
		walk := resolveRoot(root)
		if c.seenDirs[walk] {
			continue
		}
		c.seenDirs[walk] = true
		if c.scanRoot(walk) {
			found = append(found, root)
		}
	}
	out := Outcome{
		Skills:      c.skills,
		Diagnostics: c.diagnostics,
		Roots:       found,
		ScanRoots:   scans,
	}
	sort.Slice(out.Skills, func(i, j int) bool {
		if out.Skills[i].Name != out.Skills[j].Name {
			return out.Skills[i].Name < out.Skills[j].Name
		}
		return out.Skills[i].Path < out.Skills[j].Path
	})
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

// resolveRoot returns the canonical form of one root, so containment
// checks and walk bookkeeping compare real paths (macOS /var is a
// symlink to /private/var, for example). A root that cannot be
// resolved — a missing directory, say — stays cleaned; walking it is
// quietly empty.
func resolveRoot(root string) string {
	clean := filepath.Clean(root)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved
	}
	return clean
}

// cleanedRoots returns the resolved form of each root.
func cleanedRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		out = append(out, resolveRoot(root))
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
	seen        map[string]bool // canonical SKILL.md paths, collected once
	seenDirs    map[string]bool // canonical directories, walked once
	roots       []string        // cleaned scan roots for symlink containment
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
// collects every SKILL.md below it. Directories are tracked by
// canonical path, so an alias that points back into a walked tree —
// or at another root — is not re-descended: the walk stays finite
// even on a tree full of loops. A cancelled context stops the walk;
// the outcome stays partial. It reports whether the walk collected at
// least one skill.
func (c *collector) scanRoot(root string) bool {
	before := len(c.skills)
	queue := []string{root}
	for len(queue) > 0 {
		if c.ctx.Err() != nil {
			break
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
			target := full
			if entry.Type()&fs.ModeSymlink != 0 {
				// Resolve once and verify the target stays inside the
				// configured skill roots: a repo-supplied symlink must
				// never redirect discovery (or a body read) outside
				// them.
				resolved, err := filepath.EvalSymlinks(full)
				if err != nil {
					c.report(full, "resolve symlink: "+err.Error(), false)
					continue
				}
				if !insideAnyRoot(resolved, c.roots) {
					c.report(full, "symlink escapes the configured skill roots", false)
					continue
				}
				target = resolved
			}
			info, err := os.Stat(target)
			if err != nil {
				// A vanished entry, or one the kernel refuses
				// (ELOOP, ENAMETOOLONG): reporting it keeps a walk
				// that ends early from doing so silently.
				c.report(full, err.Error(), false)
				continue
			}
			if info.IsDir() {
				// Tracked by canonical path: an alias pointing back
				// into a walked tree (or at another root) is walked
				// once, which keeps the walk finite.
				if !c.seenDirs[target] {
					c.seenDirs[target] = true
					queue = append(queue, target)
				}
				continue
			}
			if name != "SKILL.md" {
				continue
			}
			// Canonicalize the file so a skill reachable through
			// several roots (or through a symlinked directory) is
			// collected once, under the path the readers resolve to.
			// Every path accepted here was containment-checked above
			// (symlinks) or is a join of canonical parents under a
			// walked root (regular entries).
			canonical := target
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
		}
	}
	return len(c.skills) > before
}
