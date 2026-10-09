package plugin

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft/internal/version"
)

const backupsDir = ".backups"

// userRoot returns the writable (non-builtin) root the scan prefers:
// the last one, which shadows every root before it. Installing into an
// earlier root would land a plugin where the scan does not look for it,
// so the two must agree on which root wins.
func (s *Store) userRoot() (string, error) {
	target := ""
	for _, root := range s.opts.Roots {
		if !root.Builtin && strings.TrimSpace(root.Path) != "" {
			target = root.Path
		}
	}
	if target != "" {
		return target, nil
	}
	return "", errdefs.Validationf(
		"plugin store: no writable plugin root configured")
}

// Inspect reads and validates one plugin source directory without
// installing it.
func (s *Store) Inspect(_ context.Context, source string) (Summary, error) {
	summary, _, err := s.inspect(source)
	return summary, err
}

// inspect is Inspect's body plus the parsed manifest, which the update
// path needs to diff an incoming package's grants against the installed
// one.
func (s *Store) inspect(source string) (Summary, Manifest, error) {
	data, err := os.ReadFile(filepath.Join(source, "plugin.json"))
	if err != nil {
		return Summary{}, Manifest{}, errdefs.Validationf(
			"plugin install: plugin.json: %v", err)
	}
	manifest, err := ParseManifest(context.Background(), data)
	if err != nil {
		return Summary{}, Manifest{}, err
	}
	if err := manifest.Validate(source); err != nil {
		return Summary{}, Manifest{}, err
	}
	return Summary{
		ID:          manifest.ID,
		Name:        manifest.Name,
		Version:     manifest.Version,
		Entry:       manifest.Entry(),
		Permissions: append([]string(nil), manifest.Permissions...),
	}, manifest, nil
}

// Install copies a plugin directory into the writable root. Installing
// over an installed plugin requires a strictly newer version and keeps
// a rollback snapshot in <root>/.backups/<id>; installing a newer
// version over an id that exists only as a builtin creates the user copy
// that shadows it, and keeps nothing, because there is no copy of ours
// to restore.
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
	if existing, ok := s.Entry(summary.ID); ok {
		if version.Compare(summary.Version, existing.Manifest.Version) <= 0 {
			return Summary{}, errdefs.Conflictf(
				"plugin install: %s version %s is not newer than %s",
				summary.ID, summary.Version, existing.Manifest.Version)
		}
		// A user copy that shadows a builtin may be newer than that
		// copy and still older than the builtin it hides: the version
		// the application ships is a floor, not a fallback.
		if existing.ShadowsBuiltin &&
			version.Compare(summary.Version, existing.BuiltinVersion) < 0 {
			return Summary{}, errdefs.Conflictf(
				"plugin install: %s version %s is older than builtin %s, which it would shadow",
				summary.ID, summary.Version, existing.BuiltinVersion)
		}
	}
	target := filepath.Join(root, summary.ID)
	// Replacing means the copy being overwritten is this one. The
	// scanned plugin may live in another root (the builtin root, most
	// likely), and then there is nothing to snapshot before the copy.
	replacing := false
	switch info, err := os.Stat(target); {
	case err == nil && info.IsDir():
		replacing = true
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return Summary{}, errdefs.Validationf(
			"plugin install: inspect %s: %v", summary.ID, err)
	}
	if replacing {
		if err := s.snapshot(root, summary.ID, target); err != nil {
			return Summary{}, err
		}
		if err := os.RemoveAll(target); err != nil {
			return Summary{}, errdefs.Validationf(
				"plugin install: replace %s: %v", summary.ID, err)
		}
	}
	if err := copyDir(source, target); err != nil {
		// A failed install leaves the previous state in place: the
		// snapshot goes back, or a fresh install leaves nothing behind.
		if !replacing {
			_ = os.RemoveAll(target)
			return Summary{}, err
		}
		if restoreErr := s.restore(root, summary.ID, target); restoreErr != nil {
			return Summary{}, errors.Join(err, restoreErr)
		}
		return Summary{}, err
	}
	s.bump()
	return summary, nil
}

// Rollback restores the last snapshot of one plugin. The id is
// validated first: every path below is built from it, and
// filepath.Join cleans "..", so an unvalidated one would resolve the
// snapshot — and the target it is swapped with — outside the plugin
// root.
func (s *Store) Rollback(ctx context.Context, id string) (Summary, error) {
	if err := ValidateID(id); err != nil {
		return Summary{}, err
	}
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

// UninstallOptions selects what survives an uninstall. Both flags are
// off by default: plugin KV and the data directory belong to the
// plugin, so a reinstall starts clean, and keeping them is a decision
// the caller makes explicitly.
type UninstallOptions struct {
	// KeepKV leaves the plugin's key/value file in place.
	KeepKV bool
	// KeepData leaves the plugin's data directory in place.
	KeepData bool
}

// Uninstall removes an installed plugin: its directory, its rollback
// snapshot, its enable state and, unless UninstallOptions keeps them,
// its KV file and data directory. A builtin plugin is refused (disable
// it instead); uninstalling a user plugin that shadows a builtin
// removes the copy and uncovers the builtin again.
//
// Nothing here stops a running plugin: Host.Uninstall drains the
// process first, and a direct caller owns that step. Application-sized
// state that happens to be keyed by plugin id (secrets, inference
// profiles) is not craft's to delete and is left alone.
func (s *Store) Uninstall(
	_ context.Context,
	id string,
	opts UninstallOptions,
) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	dir, err := s.uninstallTarget(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return errdefs.Validationf("plugin uninstall: remove %s: %v", id, err)
	}
	// Snapshots live beside the copy they belong to.
	_ = os.RemoveAll(filepath.Join(filepath.Dir(dir), backupsDir, id))
	// Gathered before the lock: the fail-closed rebuild of an unreadable
	// state file needs the surviving set, and scanning takes the lock
	// this write holds.
	survivors := s.validIDs()
	s.mu.Lock()
	if !opts.KeepKV {
		_ = os.Remove(filepath.Join(s.opts.StateDir, "kv", id+".json"))
		delete(s.kv, id)
	}
	if !opts.KeepData {
		_ = os.RemoveAll(filepath.Join(s.opts.DataDirRoot, id))
	}
	state := s.stateForWriteLocked(survivors)
	delete(state, id)
	// The plugin is gone whether or not the state write lands, so the
	// revision moves either way and every watcher reloads.
	stateErr := s.saveStateLocked(state)
	callbacks := s.bumpLocked()
	s.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
	if stateErr != nil {
		return stateErr
	}
	return nil
}

// uninstallTarget resolves the directory to remove. The scanned view
// decides, because that is the view the host serves: a builtin plugin
// is refused even when a copy sits in a writable root, and a directory
// whose manifest no longer validates still counts, since a broken
// plugin is exactly the one a user wants to remove and it never reaches
// the valid-plugin view.
func (s *Store) uninstallTarget(id string) (string, error) {
	entries, _ := s.scan()
	for _, entry := range entries {
		if entry.ID != id {
			continue
		}
		if entry.Builtin {
			return "", errdefs.Forbiddenf(
				"plugin uninstall: %s is a builtin plugin; disable it instead", id)
		}
		return entry.Dir, nil
	}
	return "", errdefs.NotFoundf("plugin uninstall: plugin %q not found", id)
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
		}
	})
}
