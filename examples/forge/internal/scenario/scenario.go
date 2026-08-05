// Package scenario owns the demo's scenario files: raid, persona, and
// test scenarios stored as plain, editable files under the forge module
// (scenarios/). No assets are embedded; forge resolves scenarios from
// disk in priority order so both source checkouts and installed
// binaries can find them.
package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Ref identifies one scenario directory on disk.
type Ref struct {
	Dir string
}

var override string

// SetOverride pins the scenario root. The forge CLI passes this through
// for --scenarios <dir>.
func SetOverride(dir string) {
	override = strings.TrimSpace(dir)
}

// Roots returns the scenario search roots in priority order:
// explicit override, FORGE_SCENARIOS, the executable's directory, the
// current working directory, and the per-user config directory.
func Roots() []string {
	var out []string
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir != "" {
			out = append(out, filepath.Clean(dir))
		}
	}
	add(override)
	add(os.Getenv("FORGE_SCENARIOS"))
	if exe, err := os.Executable(); err == nil {
		add(filepath.Join(filepath.Dir(exe), "scenarios"))
	}
	if cwd, err := os.Getwd(); err == nil {
		add(filepath.Join(cwd, "scenarios"))
	}
	if dir, err := os.UserConfigDir(); err == nil {
		add(filepath.Join(dir, "forge", "scenarios"))
	}
	return dedupe(out)
}

// Resolve finds a scenario directory by name: each root's <kind> dir
// first, then the source as a local path.
func Resolve(kind, source string) (Ref, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return Ref{}, fmt.Errorf("scenario source is required")
	}
	for _, root := range Roots() {
		dir := filepath.Join(root, kind, filepath.FromSlash(source))
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return Ref{Dir: dir}, nil
		}
	}
	if info, err := os.Stat(source); err == nil && info.IsDir() {
		return Ref{Dir: source}, nil
	}
	return Ref{}, fmt.Errorf("scenario %q not found in %s", source, strings.Join(Roots(), ", "))
}

// Copy copies every file under the scenario into dst without
// overwriting existing files.
func Copy(ref Ref, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(ref.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(ref.Dir, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return writeExclusive(filepath.Join(dst, rel), raw)
	})
}

// ReadFile reads one file out of a scenario.
func ReadFile(ref Ref, rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(ref.Dir, rel))
}

// List returns scenario directory names for a kind (raids, personas)
// across all roots, deduplicated.
func List(kind string) ([]string, error) {
	var names []string
	for _, root := range Roots() {
		dir := filepath.Join(root, kind)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				names = append(names, entry.Name())
			}
		}
	}
	return dedupeSorted(names), nil
}

// ListTests returns nested test names such as "chat/basic" across all
// roots.
func ListTests() ([]string, error) {
	var names []string
	for _, root := range Roots() {
		dir := filepath.Join(root, "tests")
		groups, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, group := range groups {
			if !group.IsDir() {
				continue
			}
			entries, err := os.ReadDir(filepath.Join(dir, group.Name()))
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.IsDir() || !isConfigFile(entry.Name()) {
					continue
				}
				names = append(names, group.Name()+"/"+configBase(entry.Name()))
			}
		}
	}
	return dedupeSorted(names), nil
}

// ReadTestSource resolves a test file (nested name or local path) and
// returns its base name and raw bytes.
func ReadTestSource(source string) (string, []byte, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil, fmt.Errorf("test source is required")
	}
	rel := normalizeTestSource(source)
	for _, root := range Roots() {
		path := filepath.Join(root, "tests", filepath.FromSlash(rel)+".yaml")
		if raw, err := os.ReadFile(path); err == nil {
			return configBase(filepath.Base(rel)), raw, nil
		}
	}
	if raw, err := os.ReadFile(source); err == nil {
		return configBase(filepath.Base(source)), raw, nil
	}
	return "", nil, fmt.Errorf("test %q not found in %s", source, strings.Join(Roots(), ", "))
}

func normalizeTestSource(source string) string {
	for _, prefix := range []string{"examples/tests/", "examples/test/", "test/", "tests/"} {
		if strings.HasPrefix(source, prefix) {
			return strings.TrimPrefix(source, prefix)
		}
	}
	return source
}

func writeExclusive(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists", path)
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func isConfigFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml", ".json":
		return true
	default:
		return false
	}
}

func configBase(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

func dedupeSorted(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func dedupe(roots []string) []string {
	return dedupeSorted(roots)
}
