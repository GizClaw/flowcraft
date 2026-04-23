package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LocalWorkspace implements Workspace backed by a local directory.
type LocalWorkspace struct {
	root string
}

// NewLocalWorkspace creates a workspace rooted at the given directory.
// The root path is resolved through EvalSymlinks to prevent the root
// itself from being a symlink that could be swapped later.
func NewLocalWorkspace(root string) (*LocalWorkspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("workspace: create root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: eval symlinks for root: %w", err)
	}
	return &LocalWorkspace{root: real}, nil
}

// Root returns the absolute path of the workspace root.
func (w *LocalWorkspace) Root() string { return w.root }

func (w *LocalWorkspace) Read(_ context.Context, path string) ([]byte, error) {
	full, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("workspace: read %s: %w", path, err)
	}
	return data, nil
}

func (w *LocalWorkspace) Write(_ context.Context, path string, data []byte) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("workspace: mkdir for %s: %w", path, err)
	}
	return os.WriteFile(full, data, 0o644)
}

func (w *LocalWorkspace) Append(_ context.Context, path string, data []byte) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("workspace: mkdir for %s: %w", path, err)
	}
	f, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("workspace: append %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(data)
	return err
}

func (w *LocalWorkspace) Rename(_ context.Context, src, dst string) error {
	srcFull, err := w.resolve(src)
	if err != nil {
		return err
	}
	dstFull, err := w.resolve(dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstFull), 0o755); err != nil {
		return fmt.Errorf("workspace: mkdir for %s: %w", dst, err)
	}
	if err := os.Rename(srcFull, dstFull); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, src)
		}
		return fmt.Errorf("workspace: rename %s -> %s: %w", src, dst, err)
	}
	return nil
}

func (w *LocalWorkspace) Delete(_ context.Context, path string) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return fmt.Errorf("workspace: delete %s: %w", path, err)
	}
	return nil
}

func (w *LocalWorkspace) RemoveAll(_ context.Context, path string) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	if full == w.root {
		return fmt.Errorf("workspace: refusing to remove root")
	}
	return os.RemoveAll(full)
}

func (w *LocalWorkspace) List(_ context.Context, dir string) ([]fs.DirEntry, error) {
	full, err := w.resolve(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, dir)
		}
		return nil, fmt.Errorf("workspace: list %s: %w", dir, err)
	}
	return entries, nil
}

func (w *LocalWorkspace) Exists(_ context.Context, path string) (bool, error) {
	full, err := w.resolve(path)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("workspace: exists %s: %w", path, err)
	}
	return true, nil
}

func (w *LocalWorkspace) Stat(_ context.Context, path string) (fs.FileInfo, error) {
	full, err := w.resolve(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("workspace: stat %s: %w", path, err)
	}
	return info, nil
}

func (w *LocalWorkspace) resolve(path string) (string, error) {
	if path == "" || path == "." {
		return w.root, nil
	}
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("%w: %s", ErrPathTraversal, path)
	}
	full := filepath.Join(w.root, cleaned)
	if !strings.HasPrefix(full, w.root+string(filepath.Separator)) && full != w.root {
		return "", fmt.Errorf("%w: %s", ErrPathTraversal, path)
	}

	// Resolve symlinks for the longest existing prefix to detect escapes
	// through symlinked intermediate directories or files.
	real, err := evalExistingPrefix(full)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve %s: %w", path, err)
	}
	if !strings.HasPrefix(real, w.root+string(filepath.Separator)) && real != w.root {
		return "", fmt.Errorf("%w: %s (symlink escape)", ErrPathTraversal, path)
	}
	return full, nil
}

// evalExistingPrefix resolves symlinks for the longest existing ancestor of
// path, then appends the remaining non-existent tail. This correctly catches
// symlink escapes even when the final target doesn't exist yet (e.g. Write
// to a new file under a symlinked directory).
func evalExistingPrefix(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	realParent, err := evalExistingPrefix(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(path)), nil
}

func (w *LocalWorkspace) GitClone(ctx context.Context, url, dest string) error {
	if err := validateGitURL(url); err != nil {
		return err
	}
	full, err := w.resolve(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("workspace: mkdir for clone dest: %w", err)
	}

	cmd := exec.CommandContext(ctx, "git", "clone", "--", url, full)
	if output, err := cmd.CombinedOutput(); err != nil {
		safe := redactURL(url)
		if safe == "" && strings.TrimSpace(url) != "" {
			safe = "<url redacted>"
		}
		return fmt.Errorf("workspace: git clone %s to %s: %w\n%s", safe, dest, err, string(output))
	}
	return nil
}

func validateGitURL(url string) error {
	trimmed := strings.TrimSpace(url)
	if trimmed == "" {
		return fmt.Errorf("workspace: git url is empty")
	}
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("workspace: git url must not start with '-'")
	}
	return nil
}

func (w *LocalWorkspace) GitPull(ctx context.Context, dir string) error {
	full, err := w.resolve(dir)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", full, "pull", "--ff-only")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("workspace: git pull in %s: %w\n%s", dir, err, string(output))
	}
	return nil
}

func (w *LocalWorkspace) GitHead(ctx context.Context, dir string) (string, error) {
	full, err := w.resolve(dir)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", full, "rev-parse", "--short", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("workspace: git rev-parse in %s: %w", dir, err)
	}
	return strings.TrimSpace(string(output)), nil
}

var _ GitWorkspace = (*LocalWorkspace)(nil)
