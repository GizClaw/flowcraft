package plugin

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeUIPlugin(t *testing.T, root, id string) string {
	t.Helper()
	writePlugin(t, root, id, `{
		"id": "`+id+`", "name": "Hello", "version": "0.1.0",
		"ui": {"entry": "dist/index.js"}
	}`)
	dir := filepath.Join(root, id)
	writeBundleFile(t, dir, "dist/index.js", "export default 1;\n")
	writeBundleFile(t, dir, "dist/chunk/two.js", "export default 2;\n")
	writeBundleFile(t, dir, "dist/logo.svg", "<svg/>")
	return dir
}

func writeBundleFile(t *testing.T, pluginDir, rel, content string) {
	t.Helper()
	path := filepath.Join(pluginDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir bundle dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write bundle file: %v", err)
	}
}

func newAssetHost(t *testing.T, root string) *Host {
	t.Helper()
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	host, err := NewHost(HostOptions{Store: store})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	return host
}

func TestAssetFSServesTheBundleTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeUIPlugin(t, root, "hello")
	host := newAssetHost(t, root)

	assets, ok := host.AssetFS("hello")
	if !ok {
		t.Fatal("AssetFS(hello) = false, want a bundle view")
	}
	index, err := fs.ReadFile(assets, "index.js")
	if err != nil {
		t.Fatalf("read index.js: %v", err)
	}
	if string(index) != "export default 1;\n" {
		t.Fatalf("index.js = %q", index)
	}
	if _, err := fs.ReadFile(assets, "chunk/two.js"); err != nil {
		t.Fatalf("read chunk/two.js: %v", err)
	}
	// The whole tree is walkable, which is what a shell that mirrors a
	// bundle into its own asset directory needs.
	var walked []string
	if err := fs.WalkDir(assets, ".", func(
		path string, entry fs.DirEntry, err error,
	) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			walked = append(walked, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk bundle: %v", err)
	}
	want := []string{"chunk/two.js", "index.js", "logo.svg"}
	if strings.Join(walked, ",") != strings.Join(want, ",") {
		t.Fatalf("walked = %v, want %v", walked, want)
	}
	entries, err := fs.ReadDir(assets, "chunk")
	if err != nil || len(entries) != 1 || entries[0].Name() != "two.js" {
		t.Fatalf("readdir chunk = %v, %v", entries, err)
	}
	info, err := fs.Stat(assets, "logo.svg")
	if err != nil || info.Size() != int64(len("<svg/>")) {
		t.Fatalf("stat logo.svg = %v, %v", info, err)
	}
	// The plugin directory is not the bundle: the manifest sits one
	// level up and is not reachable.
	if _, err := fs.ReadFile(assets, "plugin.json"); err == nil {
		t.Fatal("the bundle view reached the plugin directory")
	}
}

func TestAssetFSRefusesPathsOutsideTheBundle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pluginDir := writeUIPlugin(t, root, "hello")
	outside := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	dist := filepath.Join(pluginDir, "dist")
	mustSymlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(dist, "file-link.js"))
	mustSymlink(t, outside, filepath.Join(dist, "dir-link"))
	// A link that stays inside is a legitimate bundle technique (a
	// versioned asset pinned to a stable name), so it keeps working.
	mustSymlink(t, "index.js", filepath.Join(dist, "alias.js"))

	host := newAssetHost(t, root)
	assets, ok := host.AssetFS("hello")
	if !ok {
		t.Fatal("AssetFS(hello) = false, want a bundle view")
	}
	if data, err := fs.ReadFile(assets, "alias.js"); err != nil ||
		string(data) != "export default 1;\n" {
		t.Fatalf("internal symlink = %q, %v; want the aliased file", data, err)
	}
	cases := []struct {
		name string
		err  error
	}{
		{"", fs.ErrInvalid},
		{"./index.js", fs.ErrInvalid},
		{"../plugin.json", fs.ErrInvalid},
		{"/etc/passwd", fs.ErrInvalid},
		{"..", fs.ErrInvalid},
		{"chunk/../../plugin.json", fs.ErrInvalid},
		// A backslash is not a separator here, but it is on Windows:
		// the name must be refused on both rather than resolve
		// differently.
		{`..\..\plugin.json`, fs.ErrInvalid},
		{`chunk\..\..\plugin.json`, fs.ErrInvalid},
		{"file-link.js", ErrAssetEscape},
		{"dir-link/secret.txt", ErrAssetEscape},
		{"dir-link", ErrAssetEscape},
	}
	for _, testCase := range cases {
		_, err := fs.ReadFile(assets, testCase.name)
		if !errors.Is(err, testCase.err) {
			t.Errorf("read %q = %v, want %v", testCase.name, err, testCase.err)
		}
		var pathErr *fs.PathError
		if !errors.As(err, &pathErr) {
			t.Errorf("read %q = %v, want an *fs.PathError", testCase.name, err)
			continue
		}
		// The error echoes the name the caller spelled; a resolved path
		// would tell a plugin nothing it does not know already.
		if pathErr.Path != testCase.name {
			t.Errorf("read %q reported path %q", testCase.name, pathErr.Path)
		}
	}
}

func TestAssetFSRefusesOversizeAssets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pluginDir := writeUIPlugin(t, root, "hello")
	big := filepath.Join(pluginDir, "dist", "big.js")
	if err := os.WriteFile(big, nil, 0o600); err != nil {
		t.Fatalf("create big.js: %v", err)
	}
	if err := os.Truncate(big, MaxAssetBytes+1); err != nil {
		t.Fatalf("grow big.js: %v", err)
	}
	host := newAssetHost(t, root)
	assets, ok := host.AssetFS("hello")
	if !ok {
		t.Fatal("AssetFS(hello) = false, want a bundle view")
	}
	if _, err := fs.ReadFile(assets, "big.js"); !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("read big.js = %v, want ErrAssetTooLarge", err)
	}
	if _, err := fs.Stat(assets, "big.js"); !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("stat big.js = %v, want ErrAssetTooLarge", err)
	}
	// The open-time check is not the only one: a file that grows after
	// the shell opened it must not stream past the cap either.
	file, err := assets.Open("index.js")
	if err != nil {
		t.Fatalf("open index.js: %v", err)
	}
	defer func() { _ = file.Close() }()
	if err := os.Truncate(
		filepath.Join(pluginDir, "dist", "index.js"), MaxAssetBytes+1); err != nil {
		t.Fatalf("grow index.js: %v", err)
	}
	if _, err := io.ReadAll(file); !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("read a grown file = %v, want ErrAssetTooLarge", err)
	}
}

func TestAssetFSWithoutUI(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "headless", `{
		"id": "headless", "version": "0.1.0",
		"permissions": ["mcp:provide"], "mcp": {"command": "true"}
	}`)
	writePlugin(t, root, "broken", `{
		"id": "broken", "version": "0.1.0", "permissions": ["nope"]
	}`)
	host := newAssetHost(t, root)
	for _, id := range []string{"headless", "broken", "absent"} {
		if _, ok := host.AssetFS(id); ok {
			t.Errorf("AssetFS(%s) = true, want false", id)
		}
	}
	// A ui.entry that disappeared since the scan is not a bundle view
	// either: the view would fail on every read.
	writeUIPlugin(t, root, "hello")
	if err := os.RemoveAll(
		filepath.Join(root, "hello", "dist")); err != nil {
		t.Fatalf("remove bundle: %v", err)
	}
	if _, ok := host.AssetFS("hello"); ok {
		t.Error("AssetFS(hello) = true after the bundle was removed")
	}
}

func TestStoreAssetConfinesSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pluginDir := writeUIPlugin(t, root, "hello")
	writeBundleFile(t, pluginDir, "pets/assistant.bin", "pack")
	outside := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(outside, "secret.bin"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	mustSymlink(t, outside, filepath.Join(pluginDir, "linked"))
	host := newAssetHost(t, root)
	store := host.Store()

	data, err := store.Asset("hello", "pets/assistant.bin")
	if err != nil || string(data) != "pack" {
		t.Fatalf("Asset(pets/assistant.bin) = %q, %v", data, err)
	}
	if _, err := store.Asset("hello", "linked/secret.bin"); err == nil {
		t.Fatal("Asset followed a symlink out of the plugin directory")
	}
	if _, err := store.Asset("hello", "../plugin.json"); err == nil {
		t.Fatal("Asset accepted a path above the plugin directory")
	}
	if _, err := store.Asset("absent", "pets/assistant.bin"); err == nil {
		t.Fatal("Asset accepted an unknown plugin")
	}
	big := filepath.Join(pluginDir, "pets", "big.bin")
	if err := os.WriteFile(big, nil, 0o600); err != nil {
		t.Fatalf("create big.bin: %v", err)
	}
	if err := os.Truncate(big, MaxAssetBytes+1); err != nil {
		t.Fatalf("grow big.bin: %v", err)
	}
	if _, err := store.Asset("hello", "pets/big.bin"); err == nil {
		t.Fatal("Asset served an oversize file")
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
}
