package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// AtomicWrite writes data to path via a tmp file + Rename, so concurrent
// readers never observe a half-written payload. The tmp file is placed in
// the same directory as path so Rename stays atomic on POSIX filesystems
// (cross-directory renames are not guaranteed atomic). The tmp name is a
// private implementation detail, but it may briefly appear in List results;
// consumers that enumerate a directory should tolerate entries ending in
// ".tmp".
//
// On workspaces whose Rename is non-atomic (e.g. object stores) AtomicWrite
// still runs cleanly; durability/atomicity is then bounded by the underlying
// store's guarantees, but never weaker than a plain Write.
func AtomicWrite(ctx context.Context, ws Workspace, path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp")
	if err := ws.Write(ctx, tmp, data); err != nil {
		return fmt.Errorf("workspace atomicwrite: write tmp %s: %w", tmp, err)
	}
	if err := ws.Rename(ctx, tmp, path); err != nil {
		if dErr := ws.Delete(ctx, tmp); dErr != nil {
			// Cleanup is best-effort: the rename failure is the
			// primary error we surface, but a leftover tmp file is a
			// real disk leak, so make it visible.
			telemetry.WarnErr(ctx, "workspace atomicwrite: cleanup tmp after rename failure", dErr,
				otellog.String("op", "atomicwrite"),
				otellog.String("path", tmp))
		}
		return fmt.Errorf("workspace atomicwrite: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// Copy copies a file from src to dst within the same workspace.
func Copy(ctx context.Context, ws Workspace, src, dst string) error {
	data, err := ws.Read(ctx, src)
	if err != nil {
		return fmt.Errorf("workspace copy: read %s: %w", src, err)
	}
	if err := ws.Write(ctx, dst, data); err != nil {
		return fmt.Errorf("workspace copy: write %s: %w", dst, err)
	}
	return nil
}

// WalkFunc is the callback for Walk. Return filepath.SkipDir to skip a
// directory subtree, or any other non-nil error to abort the walk.
type WalkFunc func(path string, entry fs.DirEntry) error

// Walk recursively traverses the workspace tree rooted at dir, calling fn
// for each file and directory. Directories are visited before their contents.
func Walk(ctx context.Context, ws Workspace, dir string, fn WalkFunc) error {
	entries, err := ws.List(ctx, dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := filepath.Join(dir, entry.Name())
		if err := fn(child, entry); err != nil {
			if err == filepath.SkipDir {
				continue
			}
			return err
		}
		if entry.IsDir() {
			if err := Walk(ctx, ws, child, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// Glob returns paths matching a simple pattern relative to the workspace root.
// It supports patterns like "*.json", "dir/*.yaml", or "**/*.go".
// The "**" component matches zero or more directory levels.
func Glob(ctx context.Context, ws Workspace, pattern string) ([]string, error) {
	hasDoublestar := containsDoublestar(pattern)

	var matches []string
	err := Walk(ctx, ws, ".", func(path string, entry fs.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		// filepath.Join(".", name) is already Clean, so path carries no
		// leading "./" — it is relative to the workspace root as-is.
		var matched bool
		if hasDoublestar {
			matched = matchDoublestar(pattern, path)
		} else {
			var matchErr error
			matched, matchErr = filepath.Match(pattern, path)
			if matchErr != nil {
				return matchErr
			}
		}
		if matched {
			matches = append(matches, path)
		}
		return nil
	})
	return matches, err
}

func containsDoublestar(pattern string) bool {
	for i := 0; i+1 < len(pattern); i++ {
		if pattern[i] == '*' && pattern[i+1] == '*' {
			return true
		}
	}
	return false
}

// matchDoublestar handles patterns containing "**".
// "**" matches any number of path segments (including zero).
func matchDoublestar(pattern, path string) bool {
	parts := splitPath(pattern)
	segs := splitPath(path)
	return matchParts(parts, segs)
}

// splitPath splits p into path segments. It deliberately does NOT use
// filepath.SplitList, which splits on the OS list separator (":" on
// Unix) and would corrupt patterns containing a literal colon.
func splitPath(p string) []string {
	return split(p)
}

func split(p string) []string {
	p = filepath.Clean(p)
	if p == "." {
		return nil
	}
	var parts []string
	for {
		dir, file := filepath.Split(p)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		if dir == "" {
			break
		}
		p = dir[:len(dir)-1]
		if p == "" {
			break
		}
	}
	return parts
}

func matchParts(pattern, path []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			pattern = pattern[1:]
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(path); i++ {
				if matchParts(pattern, path[i:]) {
					return true
				}
			}
			return false
		}
		if len(path) == 0 {
			return false
		}
		matched, _ := filepath.Match(pattern[0], path[0])
		if !matched {
			return false
		}
		pattern = pattern[1:]
		path = path[1:]
	}
	return len(path) == 0
}

func cleanPath(path string) (string, error) {
	if path == "" || path == "." {
		return "", nil
	}
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("%w: %s", ErrPathTraversal, path)
	}
	return cleaned, nil
}
