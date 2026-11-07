package fsx_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
)

// Copy streams, so a 1 MiB file never becomes one buffer, and copying a file
// onto itself is refused rather than truncating the source.
func TestCopyStreamsAndRejectsTheSamePath(t *testing.T) {
	root := t.TempDir()
	tree, ctx := openTree(t, root)
	src := filepath.Join(root, "src.bin")
	dst := filepath.Join(root, "dst.bin")
	payload := bytes.Repeat([]byte("0123456789abcdef"), 64*1024) // 1 MiB
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := tree.Copy(ctx, src, dst); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("copy = %d bytes, want %d", len(got), len(payload))
	}
	if err := tree.Copy(ctx, src, src); err == nil {
		t.Fatal("Copy(src, src) = nil, want an error")
	}
}

func BenchmarkCopy(b *testing.B) {
	root := b.TempDir()
	tree, err := fsx.Open(fsx.WithWritable(root))
	if err != nil {
		b.Fatalf("fsx.Open: %v", err)
	}
	b.Cleanup(func() { _ = tree.Close() })
	src := filepath.Join(root, "src.bin")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		b.Fatal(err)
	}
	dst := filepath.Join(root, "dst.bin")
	ctx := context.Background()
	b.SetBytes(1 << 20)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := tree.Copy(ctx, src, dst); err != nil {
			b.Fatal(err)
		}
	}
}
