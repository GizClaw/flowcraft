package plugin

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Zip limits, the archive counterpart of MaxAssetBytes: one entry may
// unpack to 64 MiB, a whole package to 256 MiB. They bound the
// temporary tree an install writes before the manifest is read, so an
// archive that is not what it claims cannot fill the disk first. The
// declared sizes are the gate — archive/zip refuses to read a stream
// that runs past its declared size, so a header that lies fails the
// unpack instead of beating the cap.
const (
	MaxZipEntryBytes = 64 << 20
	MaxZipBytes      = 256 << 20
)

// zipLimits are the caps one unpack enforces. Tests shrink them to
// reach the over-limit paths with a package that fits in memory.
type zipLimits struct {
	entry int64
	total int64
}

// zipLimitsDefault is what an install enforces.
var zipLimitsDefault = zipLimits{entry: MaxZipEntryBytes, total: MaxZipBytes}

// InstallZip installs a plugin from a zip package, the shape a release
// artifact arrives in. The archive may carry the plugin files at its
// root or under a single top-level directory; the directory holding
// plugin.json is unpacked into a temporary tree and installed through
// Install, so a package gets exactly the validation, version rule,
// snapshot and rollback of a directory install.
func (s *Store) InstallZip(ctx context.Context, zipPath string) (Summary, error) {
	dir, cleanup, err := extractZip(zipPath)
	if err != nil {
		return Summary{}, err
	}
	defer cleanup()
	return s.Install(ctx, dir)
}

// extractZip unpacks one archive into a temporary directory and returns
// the directory holding plugin.json, plus the cleanup that closes the
// archive and removes the temporary tree. It is the trust boundary of a
// zip install: nothing inside the archive names a path outside that
// tree, decides that it is something other than a file or a directory,
// or unpacks past the caps.
func extractZip(zipPath string) (string, func(), error) {
	return extractZipWithin(zipPath, zipLimitsDefault)
}

// extractZipWithin is extractZip under explicit caps.
func extractZipWithin(zipPath string, limits zipLimits) (string, func(), error) {
	archive, err := zip.OpenReader(zipPath)
	// archive/zip also reports an insecure entry name, under a GODEBUG
	// that lets the check be turned off. It is ignored here rather than
	// obeyed so that the module validates names exactly the same way in
	// every environment: every entry passes through zipTarget below, and
	// that gate is stricter than the flag.
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return "", nil, errdefs.Validationf("plugin install: open zip: %v", err)
	}
	root, err := os.MkdirTemp("", "craft-plugin-*")
	if err != nil {
		_ = archive.Close()
		return "", nil, errdefs.Validationf("plugin install: temp dir: %v", err)
	}
	cleanup := func() {
		_ = archive.Close()
		_ = os.RemoveAll(root)
	}
	var total int64
	for _, entry := range archive.File {
		unpacked, err := extractEntry(root, entry, total, limits)
		if err != nil {
			cleanup()
			return "", nil, err
		}
		total += unpacked
	}
	dir, err := packageRoot(root)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

// extractEntry unpacks one archive entry under root and returns how many
// bytes it wrote.
func extractEntry(
	root string,
	entry *zip.File,
	total int64,
	limits zipLimits,
) (int64, error) {
	target, err := zipTarget(root, entry.Name)
	if err != nil {
		return 0, err
	}
	mode := entry.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		return 0, errdefs.Forbiddenf(
			"plugin install: zip entry %q is a symlink", entry.Name)
	case mode.IsDir() || strings.HasSuffix(entry.Name, "/"):
		// A trailing slash is how the format spells a directory, and
		// archives built on Windows sometimes set only that, so both
		// spellings are honored.
		if err := os.MkdirAll(target, 0o700); err != nil {
			return 0, errdefs.Validationf(
				"plugin install: zip entry %q: %v", entry.Name, err)
		}
		return 0, nil
	case !mode.IsRegular():
		return 0, errdefs.Forbiddenf(
			"plugin install: zip entry %q is not a regular file", entry.Name)
	}
	if entry.UncompressedSize64 > uint64(limits.entry) {
		return 0, errdefs.Validationf(
			"plugin install: zip entry %q unpacks to %d bytes, over the %d-byte limit",
			entry.Name, entry.UncompressedSize64, limits.entry)
	}
	if uint64(total)+entry.UncompressedSize64 > uint64(limits.total) {
		return 0, errdefs.Validationf(
			"plugin install: zip unpacks to more than %d bytes (entry %q)",
			limits.total, entry.Name)
	}
	source, err := entry.Open()
	if err != nil {
		return 0, errdefs.Validationf(
			"plugin install: open zip entry %q: %v", entry.Name, err)
	}
	defer func() { _ = source.Close() }()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, errdefs.Validationf(
			"plugin install: zip entry %q: %v", entry.Name, err)
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
		pluginFileMode(mode))
	if err != nil {
		return 0, errdefs.Validationf(
			"plugin install: zip entry %q: %v", entry.Name, err)
	}
	written, copyErr := io.Copy(file, source)
	closeErr := file.Close()
	if copyErr != nil {
		// A compressed stream that inflates past its declared size ends
		// here as zip.ErrFormat: the archive reader stops it, this is
		// just the report.
		return 0, errdefs.Validationf(
			"plugin install: unpack zip entry %q: %v", entry.Name, copyErr)
	}
	if closeErr != nil {
		return 0, errdefs.Validationf(
			"plugin install: zip entry %q: %v", entry.Name, closeErr)
	}
	return written, nil
}

// zipTarget resolves one archive entry name inside the extraction root.
// Entry names are what a hostile package gets to choose, so the gate is
// the one every other path in this module passes: slash-separated,
// relative and free of "..". A backslash is refused on top of that — a
// legal filename on Unix, a separator on Windows — and a ".." element
// is an escape rather than something to normalize away, because
// normalizing is how a name and the path it produced stop matching.
func zipTarget(root, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errdefs.Validationf("plugin install: zip entry has an empty name")
	}
	escape := errdefs.Forbiddenf(
		"plugin install: zip entry %q escapes the archive", name)
	if strings.ContainsRune(name, '\\') {
		return "", escape
	}
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || path.IsAbs(trimmed) {
		return "", escape
	}
	for _, element := range strings.Split(trimmed, "/") {
		if element == ".." {
			return "", escape
		}
	}
	target, err := ResolvePath(root, filepath.FromSlash(trimmed))
	if err != nil {
		return "", escape
	}
	return target, nil
}

// packageRoot locates the plugin inside an unpacked archive: the archive
// root when it holds plugin.json, otherwise the single top-level
// directory that does. Two candidates are ambiguous — the package does
// not say which plugin it is — and none is not a plugin package at all.
func packageRoot(root string) (string, error) {
	if pluginFile(filepath.Join(root, "plugin.json")) {
		return root, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", errdefs.Validationf("plugin install: read zip: %v", err)
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if pluginFile(filepath.Join(root, entry.Name(), "plugin.json")) {
			candidates = append(candidates, entry.Name())
		}
	}
	switch len(candidates) {
	case 0:
		return "", errdefs.Validationf("plugin install: zip has no plugin.json")
	case 1:
		return filepath.Join(root, candidates[0]), nil
	default:
		return "", errdefs.Validationf(
			"plugin install: zip holds %d plugins (%s); pack one",
			len(candidates), strings.Join(candidates, ", "))
	}
}

// pluginFile reports whether path names a regular file.
func pluginFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
