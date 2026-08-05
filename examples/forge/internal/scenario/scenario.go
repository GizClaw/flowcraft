// Package scenario owns the demo's scenario assets: embedded raid,
// persona, and test scenarios, their per-user sync, listing, and
// copying into disposable workspaces.
package scenario

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed assets
var templateFS embed.FS

// Ref identifies one scenario directory, embedded or on disk.
type Ref struct {
	Dir      string
	Embedded bool
}

// SyncUserConfigs copies embedded scenarios into the per-user forge
// config directory, only writing missing files. There is intentionally
// no environment override.
func SyncUserConfigs() error {
	root, err := userConfigDir()
	if err != nil {
		return err
	}
	return fs.WalkDir(templateFS, "assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, ok := userConfigRelPath(path)
		if !ok {
			return nil
		}
		raw, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}
		return writeIfMissing(filepath.Join(root, filepath.FromSlash(rel)), raw)
	})
}

// List returns scenario directory names (embedded + user) for a kind
// such as "raids" or "personas".
func List(kind string) ([]string, error) {
	embedded, err := listDirs(templateFS, "assets/"+kind)
	if err != nil {
		return nil, err
	}
	root, err := userConfigDir()
	if err != nil {
		return nil, err
	}
	user, err := listDirs(os.DirFS(root), filepath.Join("configs", kind))
	if err != nil {
		return nil, err
	}
	return mergeNames(embedded, user), nil
}

// ListTests returns nested test names such as "chat/basic".
func ListTests() ([]string, error) {
	embedded, err := listNested(templateFS, "assets/tests")
	if err != nil {
		return nil, err
	}
	root, err := userConfigDir()
	if err != nil {
		return nil, err
	}
	user, err := listNested(os.DirFS(root), "configs/test")
	if err != nil {
		return nil, err
	}
	return mergeNames(embedded, user), nil
}

// Resolve finds a scenario directory by name or path: embedded first,
// then the user config dir, then a local path.
func Resolve(kind, source string) (Ref, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return Ref{}, fmt.Errorf("scenario source is required")
	}
	embedded := "assets/" + kind + "/" + strings.Trim(source, "/")
	if info, err := fs.Stat(templateFS, embedded); err == nil && info.IsDir() {
		return Ref{Dir: embedded, Embedded: true}, nil
	}
	root, err := userConfigDir()
	if err != nil {
		return Ref{}, err
	}
	user := filepath.Join(root, "configs", kind, filepath.FromSlash(source))
	if info, err := os.Stat(user); err == nil && info.IsDir() {
		return Ref{Dir: user}, nil
	}
	if info, err := os.Stat(source); err == nil && info.IsDir() {
		return Ref{Dir: source}, nil
	}
	return Ref{}, fmt.Errorf("scenario %q not found (tried embedded %s, user config, and local path)", source, embedded)
}

// Copy copies every file under the scenario into dst without
// overwriting existing files.
func Copy(ref Ref, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if ref.Embedded {
		return fs.WalkDir(templateFS, ref.Dir, func(path string, d fs.DirEntry, err error) error {
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
			raw, err := fs.ReadFile(templateFS, path)
			if err != nil {
				return err
			}
			return writeExclusive(filepath.Join(dst, filepath.FromSlash(rel)), raw)
		})
	}
	return filepath.WalkDir(ref.Dir, func(path string, d fs.DirEntry, err error) error {
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
		return writeExclusive(filepath.Join(dst, filepath.FromSlash(rel)), raw)
	})
}

// ReadFile reads one file out of a scenario.
func ReadFile(ref Ref, rel string) ([]byte, error) {
	if ref.Embedded {
		return templateFS.ReadFile(ref.Dir + "/" + rel)
	}
	return os.ReadFile(filepath.Join(ref.Dir, rel))
}

// ReadTestSource resolves a test file (nested name or local path) and
// returns its base name and raw bytes.
func ReadTestSource(source string) (string, []byte, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil, fmt.Errorf("test source is required")
	}
	root, err := userConfigDir()
	if err != nil {
		return "", nil, err
	}
	if strings.Count(source, "/") == 1 {
		userPath := filepath.Join(root, "configs", "test", filepath.FromSlash(source)+".yaml")
		if raw, err := os.ReadFile(userPath); err == nil {
			return configBase(source), raw, nil
		}
		embedded := "assets/tests/" + source + ".yaml"
		if raw, err := templateFS.ReadFile(embedded); err == nil {
			return configBase(source), raw, nil
		}
	}
	for _, prefix := range []string{"test/", "tests/"} {
		if strings.HasPrefix(source, prefix) {
			rel := strings.TrimPrefix(source, prefix)
			if strings.Count(rel, "/") == 1 {
				embedded := "assets/tests/" + rel + ".yaml"
				if raw, err := templateFS.ReadFile(embedded); err == nil {
					return configBase(rel), raw, nil
				}
			}
		}
	}
	raw, fileErr := os.ReadFile(source)
	if fileErr == nil {
		return configBase(filepath.Base(source)), raw, nil
	}
	return "", nil, fmt.Errorf("test %q not found", source)
}

func userConfigDir() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(configDir, "forge"), nil
}

func userConfigRelPath(embeddedPath string) (string, bool) {
	switch {
	case strings.HasPrefix(embeddedPath, "assets/raids/"):
		return "configs/raid/" + strings.TrimPrefix(embeddedPath, "assets/raids/"), true
	case strings.HasPrefix(embeddedPath, "assets/personas/"):
		return "configs/persona/" + strings.TrimPrefix(embeddedPath, "assets/personas/"), true
	case strings.HasPrefix(embeddedPath, "assets/tests/"):
		return "configs/test/" + strings.TrimPrefix(embeddedPath, "assets/tests/"), true
	default:
		return "", false
	}
}

func writeIfMissing(path string, data []byte) error {
	_, err := os.Stat(path)
	if err == nil {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
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

func listDirs(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func listNested(fsys fs.FS, dir string) ([]string, error) {
	groups, err := fs.ReadDir(fsys, dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, group := range groups {
		if !group.IsDir() {
			continue
		}
		entries, err := fs.ReadDir(fsys, dir+"/"+group.Name())
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !isConfigFile(entry.Name()) {
				continue
			}
			out = append(out, group.Name()+"/"+configBase(entry.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
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

func mergeNames(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, item := range append(append([]string(nil), a...), b...) {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}
