package pathsafe

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWithin(t *testing.T) {
	root := filepath.FromSlash("/w/root")
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"root itself", root, true},
		{"direct child", filepath.Join(root, "a"), true},
		{"nested child", filepath.FromSlash("/w/root/a/b/c"), true},
		{"parent", filepath.FromSlash("/w"), false},
		{"sibling", filepath.FromSlash("/w/rooted"), false},
		{"uncleaned escape", filepath.FromSlash("/w/root/../other"), false},
		{"empty path", "", false},
	} {
		if got := Within(root, tc.path); got != tc.want {
			t.Errorf("%s: Within(%q, %q) = %v, want %v",
				tc.name, root, tc.path, got, tc.want)
		}
	}
	if Within("", root) {
		t.Error("an empty root must contain nothing")
	}
}

func TestWithinFold(t *testing.T) {
	root := filepath.FromSlash("/w/root")
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"exact case", root, true},
		{"folded root itself", filepath.FromSlash("/W/ROOT"), true},
		{"folded child", filepath.FromSlash("/W/root/sub"), true},
		{"folded sibling", filepath.FromSlash("/W/rooted"), false},
		{"folded parent", filepath.FromSlash("/W"), false},
		{"empty path", "", false},
	} {
		if got := WithinFold(root, tc.path); got != tc.want {
			t.Errorf("%s: WithinFold(%q, %q) = %v, want %v",
				tc.name, root, tc.path, got, tc.want)
		}
	}
	if WithinFold("", root) {
		t.Error("an empty root must contain nothing")
	}
	// The case-sensitive variant must stay selective where the
	// platform says so: folding is the caller's explicit choice. On
	// Windows filepath.Rel already compares case-insensitively, so the
	// distinction is only observable elsewhere.
	if runtime.GOOS != "windows" && Within(root, filepath.FromSlash("/W/ROOT")) {
		t.Error("Within must not fold case on a case-sensitive platform")
	}
}

func TestRel(t *testing.T) {
	root := filepath.FromSlash("/w/root")
	for _, tc := range []struct {
		name    string
		target  string
		wantRel string
		wantOK  bool
	}{
		{"root itself", root, ".", true},
		{"child", filepath.Join(root, "a"), "a", true},
		{"nested", filepath.FromSlash("/w/root/a/b"), filepath.FromSlash("a/b"), true},
		{"outside", filepath.FromSlash("/w/other"), "", false},
	} {
		rel, ok := Rel(root, tc.target)
		if ok != tc.wantOK || rel != tc.wantRel {
			t.Errorf("%s: Rel(%q, %q) = %q, %v; want %q, %v",
				tc.name, root, tc.target, rel, ok, tc.wantRel, tc.wantOK)
		}
	}
}

func TestRelRef(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{"bin/plugin", true},
		{"./bin/plugin", true},
		{"a/../b", true},
		{"..", false},
		{"../escape", false},
		{"a/../../escape", false},
		{filepath.FromSlash("/absolute"), false},
	} {
		if got := RelRef(tc.ref); got != tc.want {
			t.Errorf("RelRef(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

func TestResolveUnder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		rel  string
		want string
	}{
		{"plain", "sub/file.txt", filepath.Join(root, "sub", "file.txt")},
		{"root itself", ".", root},
		{"embedded slash", "a/b/c", filepath.Join(root, "a", "b", "c")},
	} {
		got, err := ResolveUnder(root, tc.rel)
		if err != nil {
			t.Errorf("%s: ResolveUnder(%q) error: %v", tc.name, tc.rel, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: ResolveUnder(%q) = %q, want %q",
				tc.name, tc.rel, got, tc.want)
		}
	}
	for _, rel := range []string{"../escape", "sub/../../escape", ".."} {
		if got, err := ResolveUnder(root, rel); err == nil {
			t.Errorf("ResolveUnder(%q) = %q, want an escape error", rel, got)
		}
	}
	if _, err := ResolveUnder("", "a"); err == nil {
		t.Error("ResolveUnder with an empty root must fail")
	}
}

func TestRealWithinFollowsSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "inside")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if !RealWithin(root, inside) {
		t.Error("a real file inside the root must be within it")
	}
	if !Within(root, link) {
		t.Error("lexical check accepts the link path itself")
	}
	if RealWithin(root, link) {
		t.Error("a link pointing outside the root must not be within it")
	}
	// A path that cannot be resolved falls back to its cleaned form
	// instead of silently reading as outside.
	if !RealWithin(root, filepath.Join(root, "missing")) {
		t.Error("an unresolvable child falls back to the lexical answer")
	}
}

func TestRealDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "dir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "dirlink")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if ok, err := RealDir(dir); err != nil || !ok {
		t.Errorf("RealDir(dir) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := RealDir(link); err != nil || ok {
		t.Errorf("RealDir(symlink) = %v, %v; want false, nil", ok, err)
	}
	if ok, err := RealDir(file); err != nil || ok {
		t.Errorf("RealDir(file) = %v, %v; want false, nil", ok, err)
	}
	if ok, err := RealDir(filepath.Join(base, "missing")); err != nil || ok {
		t.Errorf("RealDir(missing) = %v, %v; want false, nil", ok, err)
	}
}

// TestEscapesMatchFilepathRel pins the one expression every caller used
// to spell by hand before this package existed.
func TestEscapesMatchFilepathRel(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	for _, path := range []string{
		filepath.Join(root, "a"),
		filepath.Dir(root),
		filepath.Join(filepath.Dir(root), "sibling"),
	} {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("Rel(%q, %q): %v", root, path, err)
		}
		want := rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator))
		if got := Within(root, path); got != want {
			t.Errorf("Within(%q, %q) = %v, want %v (rel %q)",
				root, path, got, want, rel)
		}
	}
}
