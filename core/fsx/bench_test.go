package fsx_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
)

// Every file tool call resolves a path against the boundary before it touches the
// disk, and a grep or glob walks the tree once per call. These are the two costs a
// session pays for having a boundary at all.

func benchTree(b *testing.B, read, write []string) *fsx.Tree {
	b.Helper()
	tree, err := fsx.Open(read, fsx.WithWrite(write...))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = tree.Close() })
	return tree
}

func BenchmarkReadResolveAndOpen(b *testing.B) {
	dir := b.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		b.Fatalf("write fixture: %v", err)
	}
	tree := benchTree(b, []string{dir}, nil)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		if _, err := tree.Read(ctx, path); err != nil {
			b.Fatalf("Read: %v", err)
		}
	}
}

func BenchmarkWriteResolveAndCommit(b *testing.B) {
	dir := b.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	tree := benchTree(b, []string{dir}, []string{dir})
	path := filepath.Join(dir, "out.txt")
	payload := []byte("hello")
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		if err := tree.Write(ctx, path, payload); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}
}

// BenchmarkWalkSmallTree is the per-call walk a search tool pays: 64 files in eight
// directories, which is the shape of a small repository.
func BenchmarkWalkSmallTree(b *testing.B) {
	dir := b.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for i := 0; i < 8; i++ {
		sub := filepath.Join(dir, "dir"+string(rune('a'+i)))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			b.Fatalf("mkdir: %v", err)
		}
		for j := 0; j < 8; j++ {
			name := filepath.Join(sub, "file"+string(rune('a'+j))+".txt")
			if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
				b.Fatalf("write: %v", err)
			}
		}
	}
	tree := benchTree(b, []string{dir}, nil)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		count := 0
		err := tree.Walk(ctx, dir, func(string, os.DirEntry) error { count++; return nil })
		if err != nil {
			b.Fatalf("Walk: %v", err)
		}
		if count == 0 {
			b.Fatal("Walk visited nothing")
		}
	}
}
