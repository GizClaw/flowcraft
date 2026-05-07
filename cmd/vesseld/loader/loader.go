package loader

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
)

// Options controls how Load walks its inputs.
type Options struct {
	// Recursive enables descending into sub-directories. Mirrors
	// the kubectl `-R` flag. Default false matches kubectl's
	// "files in this directory only" default.
	Recursive bool
}

// Load reads every input path (file or directory) and returns the
// concatenated apispec.Object slice plus an aggregated error if
// any document failed to parse.
//
// Order rules:
//
//   - Inputs are processed in the order the caller supplied them.
//   - Within a directory, files are processed in lexicographic
//     order so configuration loads are reproducible across CI
//     runs and local dev shells.
//   - Within one file, documents are processed in document order.
//
// The resolver does not depend on order for correctness (it
// builds maps), but plan rendering and error message ordering both
// benefit from determinism.
func Load(inputs []string, opts Options) ([]apispec.Object, error) {
	var (
		objs []apispec.Object
		errs []error
	)
	for _, in := range inputs {
		got, err := loadOne(in, opts)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		objs = append(objs, got...)
	}
	if len(errs) == 0 {
		return objs, nil
	}
	return objs, joinErrors(errs)
}

// loadOne handles one input path. We resolve the path with Stat
// first so we can reject sockets / device files cleanly rather
// than trying to read them as YAML.
func loadOne(path string, opts Options) ([]apispec.Object, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("vesseld loader: stat %q: %w", path, err)
	}
	if info.Mode().IsRegular() {
		return loadFile(path)
	}
	if info.IsDir() {
		return loadDir(path, opts)
	}
	return nil, fmt.Errorf("vesseld loader: %q is not a regular file or directory (mode=%s)", path, info.Mode())
}

// loadDir walks one directory level (or recursively when -R is
// set) and concatenates the per-file results. Files whose
// extension is not in extWhitelist are skipped silently — the
// kubectl convention is that a config dir may also contain
// README.md, scripts, or other non-resource files.
func loadDir(dir string, opts Options) ([]apispec.Object, error) {
	var files []string
	walkFn := func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if !opts.Recursive && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if !isResourceExtension(path) {
			return nil
		}
		files = append(files, path)
		return nil
	}
	if opts.Recursive {
		if err := filepath.WalkDir(dir, walkFn); err != nil {
			return nil, fmt.Errorf("vesseld loader: walk %q: %w", dir, err)
		}
	} else {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("vesseld loader: read %q: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if !isResourceExtension(path) {
				continue
			}
			files = append(files, path)
		}
	}
	sort.Strings(files)

	var (
		out  []apispec.Object
		errs []error
	)
	for _, f := range files {
		got, err := loadFile(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, got...)
	}
	if len(errs) > 0 {
		return out, joinErrors(errs)
	}
	return out, nil
}

// loadFile reads a single file and decodes it. The decoder
// receives the path as the docName so error messages prefix
// with "<absolute-path>:doc[N]".
func loadFile(path string) ([]apispec.Object, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("vesseld loader: open %q: %w", path, err)
	}
	defer f.Close()
	return readAll(f, path)
}

// readAll is split out so tests can pass an io.Reader directly
// without allocating a tempfile.
func readAll(r io.Reader, name string) ([]apispec.Object, error) {
	return apispec.DecodeAll(r, name)
}

// isResourceExtension is the case-insensitive whitelist for files
// the loader will attempt to decode. Matches kubectl's behaviour:
// .yaml / .yml / .json. Anything else is silently skipped.
func isResourceExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml", ".json":
		return true
	default:
		return false
	}
}

// joinErrors flattens a slice of errors into a single error whose
// message lists each one. Avoids errors.Join because we want the
// "vesseld loader: N file(s) failed:" prefix consistent across
// callers; using errors.Join would put the join header at the
// caller's site.
func joinErrors(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	parts := make([]string, 0, len(errs)+1)
	parts = append(parts, fmt.Sprintf("vesseld loader: %d input(s) failed:", len(errs)))
	for _, e := range errs {
		parts = append(parts, "  - "+e.Error())
	}
	return fmt.Errorf("%s", strings.Join(parts, "\n"))
}
