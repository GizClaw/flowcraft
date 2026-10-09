package plugin

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// MaxAssetBytes caps one materialized plugin asset (10 MiB).
const MaxAssetBytes = 10 << 20

// Asset failures. The tree view reports them wrapped in an *fs.PathError
// whose Path is the bundle-relative name, never an absolute one.
var (
	// ErrAssetEscape reports a path that leaves the directory it is
	// served from. Only a symlink can reach it: the lexical check runs
	// first.
	ErrAssetEscape = errors.New("plugin: asset escapes the plugin directory")
	// ErrAssetTooLarge reports an asset above MaxAssetBytes.
	ErrAssetTooLarge = errors.New("plugin: asset exceeds MaxAssetBytes")
)

// AssetFS returns a read-only view of one plugin's UI bundle: the
// directory holding its ui.entry, addressed by bundle-relative paths.
// The second result is false when the id is unknown or invalid, when
// the plugin declares no ui.entry, or when the bundle is no longer
// there.
//
// The bundle is what a shell loads, so the view is rooted at the
// entry's directory rather than at the plugin directory: index.js and
// the chunks next to it resolve exactly as the bundle's own relative
// imports do. Nothing outside that directory is reachable, and every
// path is re-validated on each open because a plugin directory is
// user-writable and can change under a running shell:
//
//   - anything but a slash-separated relative clean path is fs.ErrInvalid;
//   - a symlink leaving the bundle is ErrAssetEscape, one that stays
//     inside is followed;
//   - a device, socket or fifo is fs.ErrInvalid, and a single asset
//     above MaxAssetBytes is ErrAssetTooLarge — checked when the file is
//     opened and again while it is read.
//
// ui:webview is deliberately not consulted here: craft delivers assets
// and lifecycle, and the shell gates the bundle. The permission travels
// with the ui.Registry entries.
func (h *Host) AssetFS(id string) (fs.FS, bool) {
	root, ok := h.store.bundleRoot(id)
	if !ok {
		return nil, false
	}
	return &assetFS{root: root}, true
}

// Asset materializes one plugin-relative file: a plugin file outside the
// UI bundle, such as a pet pack. Paths are relative to the plugin
// directory; for bundle files use Host.AssetFS, whose paths are relative
// to the directory holding ui.entry.
func (s *Store) Asset(id, rel string) ([]byte, error) {
	entry, ok := s.Entry(id)
	if !ok {
		return nil, errdefs.NotFoundf("plugin store: plugin %q not found", id)
	}
	path, err := ResolvePath(entry.Dir, rel)
	if err != nil {
		return nil, err
	}
	// ResolvePath is lexical. The two facts it cannot see are a symlink
	// leaving the plugin directory and an asset that grows while it is
	// read, so both are handled where the bytes are produced.
	path, err = resolveConfined(entry.Dir, path)
	switch {
	case errors.Is(err, ErrAssetEscape):
		return nil, errdefs.Forbiddenf(
			"plugin %s: asset %q escapes the plugin directory", id, rel)
	case err != nil:
		return nil, errdefs.NotFoundf("plugin %s: asset %q: %v", id, rel, err)
	}
	data, err := readAsset(path)
	switch {
	case errors.Is(err, ErrAssetTooLarge):
		return nil, errdefs.Validationf(
			"plugin %s: asset %q exceeds %d bytes", id, rel, MaxAssetBytes)
	case errors.Is(err, fs.ErrInvalid):
		return nil, errdefs.Validationf(
			"plugin %s: asset %q is not a regular file", id, rel)
	case err != nil:
		return nil, errdefs.NotFoundf("plugin %s: asset %q: %v", id, rel, err)
	}
	return data, nil
}

// bundleRoot resolves the directory holding one plugin's ui.entry.
func (s *Store) bundleRoot(id string) (string, bool) {
	entry, ok := s.Entry(id)
	if !ok {
		return "", false
	}
	rel := strings.TrimSpace(entry.Manifest.Entry())
	if rel == "" {
		return "", false
	}
	// The manifest was validated when the plugin was scanned; the
	// resolution is repeated here because the tree may have changed
	// since, and a stale manifest must not become a stale root.
	path, err := ResolvePath(entry.Dir, rel)
	if err != nil {
		return "", false
	}
	resolved, err := resolveConfined(entry.Dir, path)
	if err != nil {
		return "", false
	}
	return filepath.Dir(resolved), true
}

// resolveConfined resolves path inside root, following symlinks only
// while they stay inside: a link pointing out of root is an escape, not
// a redirect. Both sides are symlink-resolved first, so a plugin reached
// through a symlinked root still resolves.
func resolveConfined(root, path string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !within(resolvedRoot, resolved) {
		return "", ErrAssetEscape
	}
	return resolved, nil
}

// within reports whether path stays inside root. Both must be absolute
// and symlink-resolved; the comparison is lexical.
func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// readAsset reads one confined plugin file with the size cap applied
// while reading, so an oversized file is refused without being
// materialized first.
func readAsset(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	// Checked before the open, not after: opening a fifo blocks until a
	// writer appears, and no asset is worth a host stuck on one.
	if !info.Mode().IsRegular() {
		return nil, fs.ErrInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, MaxAssetBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxAssetBytes {
		return nil, ErrAssetTooLarge
	}
	return data, nil
}

// assetFS is the bundle view a shell iterates. It is stateless: the root
// is fixed, everything else is answered from the filesystem.
type assetFS struct{ root string }

var (
	_ fs.FS        = (*assetFS)(nil)
	_ fs.StatFS    = (*assetFS)(nil)
	_ fs.ReadDirFS = (*assetFS)(nil)
)

// Open opens one bundle-relative path.
func (a *assetFS) Open(name string) (fs.File, error) {
	path, info, err := a.resolve("open", name)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, assetError("open", name, fs.ErrNotExist)
	}
	if info.IsDir() {
		// *os.File is also an fs.ReadDirFile, so a directory opened
		// directly lists as one reached through ReadDir.
		return file, nil
	}
	return &assetFile{File: file, remaining: MaxAssetBytes}, nil
}

// Stat returns the metadata of one bundle path.
func (a *assetFS) Stat(name string) (fs.FileInfo, error) {
	_, info, err := a.resolve("stat", name)
	return info, err
}

// ReadDir lists one bundle directory, sorted by name.
func (a *assetFS) ReadDir(name string) ([]fs.DirEntry, error) {
	path, info, err := a.resolve("readdir", name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, assetError("readdir", name, fs.ErrInvalid)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, assetError("readdir", name, fs.ErrNotExist)
	}
	return entries, nil
}

// resolve validates one bundle-relative name and returns the file it
// names together with its metadata.
func (a *assetFS) resolve(op, name string) (string, fs.FileInfo, error) {
	// ValidPath already rejects "", ".", "..", absolute paths and any
	// path carrying a ".." element. A backslash is refused on top of
	// that: it is a legal filename on Unix but a separator on Windows,
	// and a bundle is meant to be portable, so a name containing one is
	// invalid everywhere instead of an escape somewhere.
	if !fs.ValidPath(name) || strings.ContainsRune(name, '\\') {
		return "", nil, assetError(op, name, fs.ErrInvalid)
	}
	path := a.root
	if name != "." {
		path = filepath.Join(a.root, filepath.FromSlash(name))
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, assetError(op, name, fs.ErrNotExist)
	}
	if !within(a.root, resolved) {
		return "", nil, assetError(op, name, ErrAssetEscape)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, assetError(op, name, fs.ErrNotExist)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return "", nil, assetError(op, name, fs.ErrInvalid)
	}
	if info.Mode().IsRegular() && info.Size() > MaxAssetBytes {
		return "", nil, assetError(op, name, ErrAssetTooLarge)
	}
	return resolved, info, nil
}

// assetError tags one bundle failure with the name as the caller spelled
// it.
func assetError(op, name string, err error) error {
	return &fs.PathError{Op: op, Path: name, Err: err}
}

// assetFile bounds reads, so a file that grows after it was opened
// cannot stream past the cap it passed at open time.
type assetFile struct {
	*os.File
	remaining int64
}

func (f *assetFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.remaining -= int64(n)
	if f.remaining < 0 {
		return n, ErrAssetTooLarge
	}
	return n, err
}
