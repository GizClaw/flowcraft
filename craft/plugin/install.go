package plugin

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft/internal/version"
)

const backupsDir = ".backups"

// userRoot returns the first writable (non-builtin) root.
func (s *Store) userRoot() (string, error) {
	for _, root := range s.opts.Roots {
		if !root.Builtin && strings.TrimSpace(root.Path) != "" {
			return root.Path, nil
		}
	}
	return "", errdefs.Validationf(
		"plugin store: no writable plugin root configured")
}

// Inspect reads and validates one plugin source directory without
// installing it.
func (s *Store) Inspect(_ context.Context, source string) (Summary, error) {
	data, err := os.ReadFile(filepath.Join(source, "plugin.json"))
	if err != nil {
		return Summary{}, errdefs.Validationf(
			"plugin install: plugin.json: %v", err)
	}
	manifest, err := ParseManifest(context.Background(), data)
	if err != nil {
		return Summary{}, err
	}
	if err := manifest.Validate(source); err != nil {
		return Summary{}, err
	}
	return Summary{
		ID:          manifest.ID,
		Name:        manifest.Name,
		Version:     manifest.Version,
		Entry:       manifest.Entry(),
		Permissions: append([]string(nil), manifest.Permissions...),
	}, nil
}

// Install copies a plugin directory into the writable root. Installing
// over an existing plugin requires a strictly newer version and keeps a
// rollback snapshot in <root>/.backups/<id>.
func (s *Store) Install(
	ctx context.Context,
	source string,
) (Summary, error) {
	summary, err := s.Inspect(ctx, source)
	if err != nil {
		return Summary{}, err
	}
	root, err := s.userRoot()
	if err != nil {
		return Summary{}, err
	}
	target := filepath.Join(root, summary.ID)
	if existing, ok := s.Entry(summary.ID); ok && existing.Error == "" {
		if version.Compare(summary.Version, existing.Manifest.Version) <= 0 {
			return Summary{}, errdefs.Conflictf(
				"plugin install: %s version %s is not newer than %s",
				summary.ID, summary.Version, existing.Manifest.Version)
		}
		if err := s.snapshot(root, summary.ID, target); err != nil {
			return Summary{}, err
		}
		if err := os.RemoveAll(target); err != nil {
			return Summary{}, errdefs.Validationf(
				"plugin install: replace %s: %v", summary.ID, err)
		}
	}
	if err := copyDir(source, target); err != nil {
		if err := s.restore(root, summary.ID, target); err != nil {
			return Summary{}, err
		}
		return Summary{}, err
	}
	s.bump()
	return summary, nil
}

// Rollback restores the last snapshot of one plugin.
func (s *Store) Rollback(ctx context.Context, id string) (Summary, error) {
	root, err := s.userRoot()
	if err != nil {
		return Summary{}, err
	}
	backup := filepath.Join(root, backupsDir, id)
	if info, err := os.Stat(backup); err != nil || !info.IsDir() {
		return Summary{}, errdefs.NotFoundf(
			"plugin install: no rollback snapshot for %q", id)
	}
	target := filepath.Join(root, id)
	trash := target + ".rollback-trash"
	_ = os.RemoveAll(trash)
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, trash); err != nil {
			return Summary{}, errdefs.Validationf(
				"plugin install: stash %s: %v", id, err)
		}
	}
	if err := os.Rename(backup, target); err != nil {
		_ = os.Rename(trash, target)
		return Summary{}, errdefs.Validationf(
			"plugin install: restore %s: %v", id, err)
	}
	_ = os.RemoveAll(trash)
	s.bump()
	return s.Inspect(ctx, target)
}

func (s *Store) snapshot(root, id, target string) error {
	backup := filepath.Join(root, backupsDir, id)
	_ = os.RemoveAll(backup)
	if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
		return errdefs.Validationf("plugin install: backup dir: %v", err)
	}
	if err := copyDir(target, backup); err != nil {
		return err
	}
	return nil
}

func (s *Store) restore(root, id, target string) error {
	backup := filepath.Join(root, backupsDir, id)
	if _, err := os.Stat(backup); err != nil {
		return nil
	}
	_ = os.RemoveAll(target)
	return copyDir(backup, target)
}

func (s *Store) bump() {
	s.mu.Lock()
	callbacks := s.bumpLocked()
	s.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
}

// pluginFileMode is the mode a plugin file is written with: 0600, or
// 0700 when the source is executable. A plugin may ship a compiled MCP
// server, and dropping the executable bit would install a package whose
// own command cannot start; everything else stays closed, because a
// plugin tree is user-installed content that no one else needs to read.
func pluginFileMode(mode fs.FileMode) fs.FileMode {
	if mode&0o111 != 0 {
		return 0o700
	}
	return 0o600
}

// copyDir copies a plugin directory tree, rejecting symlinks.
func copyDir(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return errdefs.Validationf("plugin install: walk: %v", err)
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return errdefs.Validationf("plugin install: rel: %v", err)
		}
		destination := filepath.Join(target, relative)
		switch {
		case entry.Type()&os.ModeSymlink != 0:
			return errdefs.Forbiddenf(
				"plugin install: symlink %q is not allowed", relative)
		case entry.IsDir():
			if err := os.MkdirAll(destination, 0o700); err != nil {
				return errdefs.Validationf("plugin install: mkdir: %v", err)
			}
			return nil
		default:
			info, err := entry.Info()
			if err != nil {
				return errdefs.Validationf("plugin install: stat: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return errdefs.Validationf("plugin install: read: %v", err)
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return errdefs.Validationf("plugin install: mkdir: %v", err)
			}
			if err := os.WriteFile(destination, data, pluginFileMode(info.Mode())); err != nil {
				return errdefs.Validationf("plugin install: write: %v", err)
			}
			return nil
			return nil
		}
	})
}
