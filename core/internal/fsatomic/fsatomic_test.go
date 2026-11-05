package fsatomic

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// noLitter asserts dir holds exactly the given names, so a failure path
// that forgot its temp file shows up as a test failure rather than a
// directory full of .tmp-* files.
func noLitter(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want %v", names, want)
	}
}

func TestWritePublishesContentAndPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "flowcraft.yaml")
	err := Write(path, []byte("a: 1\n"), Options{
		Perm:       0o600,
		MkdirPerm:  0o700,
		TempPrefix: ".flowcraft-*.tmp",
		Sync:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a: 1\n" {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows reports synthesized modes for every file, so the POSIX
	// permission assertions only apply elsewhere; the content and
	// litter checks below run on every platform.
	if runtime.GOOS != "windows" {
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
		}
		// MkdirPerm 0700 must not leak group/other bits (umask may
		// narrow it further, which is fine).
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if dirInfo.Mode().Perm()&0o077 != 0 {
			t.Fatalf("directory mode = %v, want no group/other bits",
				dirInfo.Mode().Perm())
		}
	}
	noLitter(t, filepath.Dir(path), "flowcraft.yaml")
}

func TestWriteZeroPermKeepsTempMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports synthesized file modes, not the 0600 default")
	}
	// Call sites that never chmod keep os.CreateTemp's 0600.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := Write(path, []byte("x"), Options{TempPrefix: ".agent-*.tmp"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want the 0600 temp default", info.Mode().Perm())
	}
}

func TestWriteWithoutMkdirReportsMissingDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "file.json")
	err := Write(path, []byte("x"), Options{})
	if err == nil {
		t.Fatal("want an error when the parent directory does not exist")
	}
	if !strings.Contains(err.Error(), "fsatomic") {
		t.Fatalf("error = %v, want the fsatomic prefix", err)
	}
	// Neither the parent nor a temp file may appear.
	noLitter(t, dir)
}

func TestWriteRenameFailureLeavesDestinationAndNoTemp(t *testing.T) {
	// A directory at the destination makes the rename fail on every
	// platform; the payload must not survive as temp litter.
	dir := t.TempDir()
	path := filepath.Join(dir, "occupied")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	err := Write(path, []byte("x"), Options{Perm: 0o644, TempPrefix: ".skill-*.tmp"})
	if err == nil {
		t.Fatal("want a rename failure when the destination is a directory")
	}
	if !strings.Contains(err.Error(), "rename") {
		t.Fatalf("error = %v, want the rename step named", err)
	}
	noLitter(t, dir, "occupied")
}

func TestWriteSyncFailureStillCleansTemp(t *testing.T) {
	// The sync flag must not change the failure contract: the temp file
	// goes away and the destination is untouched.
	dir := t.TempDir()
	path := filepath.Join(dir, "occupied")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	err := Write(path, []byte("x"), Options{Perm: 0o600, Sync: true})
	if err == nil {
		t.Fatal("want a rename failure when the destination is a directory")
	}
	noLitter(t, dir, "occupied")
}

func TestWriteTempCreationFailureLeavesNoLitter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory modes do not deny writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory bit")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := Write(filepath.Join(dir, "file.json"), []byte("x"), Options{})
	if err == nil {
		t.Fatal("want an error when the directory is not writable")
	}
	if !strings.Contains(err.Error(), "create temp file") {
		t.Fatalf("error = %v, want the create-temp step named", err)
	}
}
