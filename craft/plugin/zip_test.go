package plugin

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// zipEntry is one file in a test package. A zero Mode is a regular
// 0644 file; a trailing slash in the name is enough to make a
// directory, which is how the format spells one.
type zipEntry struct {
	name string
	data string
	mode fs.FileMode
}

// rawEntry is one entry whose header declares sizes of its own, the
// shape a hostile package uses. Payload is written verbatim after the
// header, so a caller hands over an already-compressed stream.
type rawEntry struct {
	name     string
	unpacked uint64
	method   uint16
	payload  []byte
}

// writeZip packs entries into a zip file and returns its path.
func writeZip(t *testing.T, dir string, entries ...zipEntry) string {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(mode)
		out, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("zip entry %q: %v", entry.name, err)
		}
		if _, err := out.Write([]byte(entry.data)); err != nil {
			t.Fatalf("zip entry %q: %v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return writeArchive(t, dir, buffer.Bytes())
}

// writeRawZip packs entries without letting archive/zip compute their
// sizes from the data.
func writeRawZip(t *testing.T, dir string, entries ...rawEntry) string {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{
			Name:               entry.name,
			Method:             entry.method,
			CompressedSize64:   uint64(len(entry.payload)),
			UncompressedSize64: entry.unpacked,
		}
		header.SetMode(0o644)
		out, err := writer.CreateRaw(header)
		if err != nil {
			t.Fatalf("zip entry %q: %v", entry.name, err)
		}
		if _, err := out.Write(entry.payload); err != nil {
			t.Fatalf("zip entry %q: %v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return writeArchive(t, dir, buffer.Bytes())
}

// writeArchive saves archive bytes under a unique name and returns its
// path.
func writeArchive(t *testing.T, dir string, data []byte) string {
	t.Helper()
	file, err := os.CreateTemp(dir, "*.zip")
	if err != nil {
		t.Fatalf("zip file: %v", err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatalf("zip file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("zip file: %v", err)
	}
	return file.Name()
}

// deflate compresses data, the payload of a raw entry.
func deflate(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer, err := flate.NewWriter(&out, flate.BestSpeed)
	if err != nil {
		t.Fatalf("flate: %v", err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return out.Bytes()
}

// TestInstallZipPackagesAccepted covers the two shapes a release
// artifact arrives in: plugin.json at the root of the archive, and the
// files under a single top-level directory. Both install through the
// directory pipeline, so a package behaves exactly like the directory it
// was packed from: same summary, same replace semantics.
func TestInstallZipPackagesAccepted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newRootStore(t, root)
	dir := t.TempDir()
	revision := store.Revision()

	flat := writeZip(t, dir,
		zipEntry{name: "plugin.json", data: manifestFor("hello", "0.1.0")},
		zipEntry{name: "server/main.py", data: "print(1)\n"},
		zipEntry{name: "stale.txt", data: "gone after the update\n"},
	)
	summary, err := store.InstallZip(context.Background(), flat)
	if err != nil || summary.Version != "0.1.0" {
		t.Fatalf("InstallZip = %+v, %v", summary, err)
	}
	if summary.ID != "hello" || summary.Entry != "" {
		t.Fatalf("summary = %+v, want the manifest's own fields", summary)
	}
	if body, err := os.ReadFile(
		filepath.Join(root, "hello", "server", "main.py")); err != nil ||
		string(body) != "print(1)\n" {
		t.Fatalf("unpacked file = %q, %v", body, err)
	}

	nested := writeZip(t, dir,
		zipEntry{name: "hello-0.2.0/plugin.json", data: manifestFor("hello", "0.2.0")},
		zipEntry{name: "hello-0.2.0/server/main.py", data: "print(2)\n"},
	)
	summary, err = store.InstallZip(context.Background(), nested)
	if err != nil || summary.Version != "0.2.0" {
		t.Fatalf("InstallZip nested = %+v, %v", summary, err)
	}
	if store.Revision() != revision+2 {
		t.Fatalf("revision = %d, want two installs after %d",
			store.Revision(), revision)
	}
	if _, err := os.Stat(
		filepath.Join(root, "hello", "stale.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("file of the previous version survived the update: %v", err)
	}
	if body, err := os.ReadFile(
		filepath.Join(root, "hello", "server", "main.py")); err != nil ||
		string(body) != "print(2)\n" {
		t.Fatalf("updated file = %q, %v", body, err)
	}
}

// TestInstallZipMatchesDirectoryInstall pins the claim the surface
// makes: a package and the directory it was packed from install to the
// same summary.
func TestInstallZipMatchesDirectoryInstall(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writePlugin(t, source, "hello", manifestFor("hello", "0.1.0"))

	fromDirectory := newRootStore(t, t.TempDir())
	directorySummary, err := fromDirectory.Install(context.Background(),
		filepath.Join(source, "hello"))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	fromZip := newRootStore(t, t.TempDir())
	zipSummary, err := fromZip.InstallZip(context.Background(),
		writeZip(t, t.TempDir(), zipEntry{
			name: "plugin.json", data: manifestFor("hello", "0.1.0")}))
	if err != nil {
		t.Fatalf("InstallZip: %v", err)
	}
	if !reflect.DeepEqual(directorySummary, zipSummary) {
		t.Fatalf("directory install = %+v, zip install = %+v",
			directorySummary, zipSummary)
	}
}

// TestInstallZipRejectsHostileArchives covers the archive itself as the
// trust boundary: an entry may not name a path outside the extraction
// tree and may not claim to be something other than a file or a
// directory. Nothing is installed on the way out, and the previous
// version of the plugin is left exactly as it was.
func TestInstallZipRejectsHostileArchives(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries []zipEntry
		want    string
	}{
		{
			name: "parent traversal",
			entries: []zipEntry{
				{name: "../escape.txt", data: "x"},
				{name: "plugin.json", data: manifestFor("hello", "0.2.0")},
			},
			want: "escapes the archive",
		},
		{
			name: "traversal in the middle",
			entries: []zipEntry{
				{name: "server/../../escape.txt", data: "x"},
				{name: "plugin.json", data: manifestFor("hello", "0.2.0")},
			},
			want: "escapes the archive",
		},
		{
			name: "traversal that would normalize away",
			entries: []zipEntry{
				{name: "rename/../plugin.json", data: manifestFor("hello", "0.2.0")},
			},
			want: "escapes the archive",
		},
		{
			name: "absolute path",
			entries: []zipEntry{
				{name: "/tmp/escape.txt", data: "x"},
				{name: "plugin.json", data: manifestFor("hello", "0.2.0")},
			},
			want: "escapes the archive",
		},
		{
			name: "windows separator",
			entries: []zipEntry{
				{name: `server\..\..\escape.txt`, data: "x"},
				{name: "plugin.json", data: manifestFor("hello", "0.2.0")},
			},
			want: "escapes the archive",
		},
		{
			name: "symlink",
			entries: []zipEntry{
				{name: "link", data: "/etc", mode: fs.ModeSymlink | 0o777},
				{name: "plugin.json", data: manifestFor("hello", "0.2.0")},
			},
			want: "symlink",
		},
		{
			name: "named pipe",
			entries: []zipEntry{
				{name: "pipe", mode: fs.ModeNamedPipe | 0o600},
				{name: "plugin.json", data: manifestFor("hello", "0.2.0")},
			},
			want: "not a regular file",
		},
		{
			name: "file where a directory is needed",
			entries: []zipEntry{
				{name: "plugin.json", data: manifestFor("hello", "0.2.0")},
				{name: "server", data: "not a directory"},
				{name: "server/main.py", data: "print(1)\n"},
			},
			want: "server",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			store := newRootStore(t, root)
			source := t.TempDir()
			writePlugin(t, source, "hello", manifestFor("hello", "0.1.0"))
			if _, err := store.Install(context.Background(),
				filepath.Join(source, "hello")); err != nil {
				t.Fatalf("install 0.1.0: %v", err)
			}
			if err := store.SetEnabled("hello", false); err != nil {
				t.Fatalf("SetEnabled: %v", err)
			}
			revision := store.Revision()

			_, err := store.InstallZip(context.Background(),
				writeZip(t, t.TempDir(), testCase.entries...))
			if err == nil {
				t.Fatal("hostile archive was installed")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want it to mention %q", err, testCase.want)
			}
			entry, ok := store.Entry("hello")
			if !ok || entry.Manifest.Version != "0.1.0" {
				t.Fatalf("installed plugin after the refusal = %+v, %v", entry, ok)
			}
			if entry.Enabled {
				t.Fatal("enable state was touched by a failed install")
			}
			if store.Revision() != revision {
				t.Fatalf("revision moved to %d on a failed install",
					store.Revision())
			}
		})
	}
}

// TestInstallZipLimits covers the size caps. Declared sizes are the
// gate, so a package that claims to unpack past a cap is refused before
// anything is written.
func TestInstallZipLimits(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newRootStore(t, root)
	// One entry above the cap, declared by its own header: the install
	// must be refused before a single byte is unpacked.
	_, err := store.InstallZip(context.Background(), writeRawZip(t, t.TempDir(), rawEntry{
		name:     "huge.bin",
		unpacked: MaxZipEntryBytes + 1,
		payload:  []byte("x"),
	}))
	if err == nil {
		t.Fatal("an entry above the cap was accepted")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Fatalf("error = %v, want it to name the limit", err)
	}
	if entries, err := store.Entries(); err != nil || len(entries) != 0 {
		t.Fatalf("store after the refusal = %+v, %v", entries, err)
	}
}

// TestExtractZipLimits reaches the two caps with a package small enough
// to build in memory: the caps are the ones an install uses, read from
// the archive headers, so shrinking them proves the check rather than
// the fixture.
func TestExtractZipLimits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		limits  zipLimits
		entries []zipEntry
		want    string
	}{
		{
			name:    "entry above the per-entry cap",
			limits:  zipLimits{entry: 8, total: 32},
			entries: []zipEntry{{name: "plugin.json", data: "0123456789"}},
			want:    "over the",
		},
		{
			name:   "package above the per-package cap",
			limits: zipLimits{entry: 11, total: 12},
			entries: []zipEntry{
				{name: "plugin.json", data: "0123456789"},
				{name: "server.py", data: "0123"},
			},
			want: "more than",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, cleanup, err := extractZipWithin(
				writeZip(t, t.TempDir(), testCase.entries...), testCase.limits)
			if cleanup != nil {
				t.Fatal("a failed unpack returned a cleanup func")
			}
			if err == nil {
				t.Fatal("an oversized package was unpacked")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

// TestInstallZipRefusesAnUnderstatedEntry pins why the caps can be read
// off the headers: a package that declares a small entry and ships a
// compressed stream that inflates past that size is stopped by the
// archive reader, not by the cap. The unpack fails and the store is
// untouched.
func TestInstallZipRefusesAnUnderstatedEntry(t *testing.T) {
	t.Parallel()
	payload := deflate(t, make([]byte, 1<<20))
	root := t.TempDir()
	store := newRootStore(t, root)
	_, err := store.InstallZip(context.Background(), writeRawZip(t, t.TempDir(), rawEntry{
		name:     "plugin.json",
		unpacked: 8,
		method:   zip.Deflate,
		payload:  payload,
	}))
	if err == nil {
		t.Fatal("an entry that inflated past its declared size was accepted")
	}
	if !strings.Contains(err.Error(), "plugin.json") {
		t.Fatalf("error = %v, want it to name the entry", err)
	}
	if entries, err := store.Entries(); err != nil || len(entries) != 0 {
		t.Fatalf("store after the refusal = %+v, %v", entries, err)
	}
}

// TestInstallZipWithoutAPlugin covers the packages that are not a plugin
// at all: no manifest anywhere, and two candidate plugins under one
// archive.
func TestInstallZipWithoutAPlugin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries []zipEntry
		want    string
	}{
		{
			name:    "no manifest",
			entries: []zipEntry{{name: "readme.md", data: "hello"}},
			want:    "has no plugin.json",
		},
		{
			name: "two candidates",
			entries: []zipEntry{
				{name: "one/plugin.json", data: manifestFor("one", "0.1.0")},
				{name: "two/plugin.json", data: manifestFor("two", "0.1.0")},
			},
			want: "holds 2 plugins",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			store := newRootStore(t, root)
			_, err := store.InstallZip(context.Background(),
				writeZip(t, t.TempDir(), testCase.entries...))
			if err == nil {
				t.Fatal("archive without a single plugin was installed")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want it to mention %q", err, testCase.want)
			}
			if entries, err := store.Entries(); err != nil || len(entries) != 0 {
				t.Fatalf("store after the refusal = %+v, %v", entries, err)
			}
		})
	}
}

// TestInstallZipVersionRules pins that a package obeys the same version
// rule as a directory: over an installed plugin it must be strictly
// newer, and an equal or older one is refused as a conflict.
func TestInstallZipVersionRules(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newRootStore(t, root)
	dir := t.TempDir()
	for _, version := range []string{"0.1.0", "0.2.0"} {
		path := writeZip(t, dir, zipEntry{
			name: "plugin.json", data: manifestFor("hello", version)})
		if _, err := store.InstallZip(context.Background(), path); err != nil {
			t.Fatalf("InstallZip %s: %v", version, err)
		}
	}
	for _, version := range []string{"0.2.0", "0.1.0"} {
		path := writeZip(t, dir, zipEntry{
			name: "plugin.json", data: manifestFor("hello", version)})
		_, err := store.InstallZip(context.Background(), path)
		if !errdefs.IsConflict(err) {
			t.Fatalf("InstallZip %s = %v, want Conflict", version, err)
		}
	}
	entry, ok := store.Entry("hello")
	if !ok || entry.Manifest.Version != "0.2.0" {
		t.Fatalf("entry after refusals = %+v, %v", entry, ok)
	}
}

// TestInstallZipKeepsExecutableBit pins the one mode a package carries
// that matters: a bundled server that arrives with the executable bit
// must stay executable, or the package installs into something whose own
// command cannot start.
func TestInstallZipKeepsExecutableBit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newRootStore(t, root)
	_, err := store.InstallZip(context.Background(), writeZip(t, t.TempDir(),
		zipEntry{name: "plugin.json", data: manifestFor("hello", "0.1.0")},
		zipEntry{name: "server/run", data: "#!/bin/sh\n", mode: 0o755},
		zipEntry{name: "server/read.py", data: "print(1)\n", mode: 0o644},
	))
	if err != nil {
		t.Fatalf("InstallZip: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "hello", "server", "run"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("executable entry = %v, %v", info, err)
	}
	info, err = os.Stat(filepath.Join(root, "hello", "server", "read.py"))
	if err != nil || info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("plain entry = %v, %v", info, err)
	}
}
