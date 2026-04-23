package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
)

// AtomicWrite writes data to path via a tmp file + Rename, so concurrent
// readers never observe a half-written payload. The tmp file is placed in
// the same directory as path so Rename stays atomic on POSIX filesystems
// (cross-directory renames are not guaranteed atomic).
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
		_ = ws.Delete(ctx, tmp)
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
		rel := path
		if len(rel) > 2 && rel[:2] == "./" {
			rel = rel[2:]
		}

		var matched bool
		if hasDoublestar {
			matched = matchDoublestar(pattern, rel)
		} else {
			var matchErr error
			matched, matchErr = filepath.Match(pattern, rel)
			if matchErr != nil {
				return matchErr
			}
		}
		if matched {
			matches = append(matches, rel)
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

func splitPath(p string) []string {
	var parts []string
	for _, s := range filepath.SplitList(p) {
		for _, seg := range split(s) {
			parts = append(parts, seg)
		}
	}
	return parts
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
