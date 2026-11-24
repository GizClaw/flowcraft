//go:build darwin

package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// benchRereadDirectory times one re-read of a directory of the given
// shape, both ways: the scan this source does now, which takes each
// entry as the directory's own records describe it, and the scan it did
// before, which is the same read plus a stat per entry. Both are timed
// on one directory in one run, so the two numbers are worth comparing to
// each other rather than to the machine that produced them.
//
// A re-read is what one directory note costs, and a note arriving in a
// wide directory is the moment a watcher that cannot keep up falls
// behind — which is why this is the number that matters here.
//
// Run with:
//
//	go test -bench=BenchmarkDarwinRereadDirectory -benchmem ./sandbox/journal
func benchRereadDirectory(b *testing.B, entries int, dirs bool) {
	root := b.TempDir()
	for i := range entries {
		name := filepath.Join(root, fmt.Sprintf("entry%05d", i))
		var err error
		if dirs {
			err = os.Mkdir(name, 0o755)
		} else {
			err = os.WriteFile(name, []byte("x"), 0o644)
		}
		if err != nil {
			b.Fatal(err)
		}
	}
	src, err := openSource()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = src.Close() })
	handle, err := src.Add(root)
	if err != nil {
		b.Fatal(err)
	}
	w := src.(*kqueueSource).dirs[handle]

	b.ReportAllocs()
	b.Run("records", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := w.read(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("stat-per-entry", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			scanStatPerEntry(b, w)
		}
	})
}

// BenchmarkDarwinRereadDirectoryWideFiles is the shape that made the old
// scan expensive: a flat directory holding thousands of files, which is
// what a build output, an unarchived bundle or a package cache looks
// like. The records answer for every entry here, so the distance between
// the two numbers is the stat per entry that is no longer taken.
func BenchmarkDarwinRereadDirectoryWideFiles(b *testing.B) {
	benchRereadDirectory(b, 4096, false)
}

// BenchmarkDarwinRereadDirectoryOfDirectories is the remaining cost,
// stated rather than left to be found: a directory entry is still
// stat'ed, because only a stat sees a volume mounted over a name. A flat
// directory of directories keeps paying one stat per entry, and the two
// numbers meet again.
func BenchmarkDarwinRereadDirectoryOfDirectories(b *testing.B) {
	benchRereadDirectory(b, 2048, true)
}
