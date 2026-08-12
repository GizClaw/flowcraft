package resource

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestLoaderInline(t *testing.T) {
	data, err := NewLoader().Load(
		context.Background(), Source{Inline: []byte("a: 1")})
	if err != nil || string(data) != "a: 1" {
		t.Fatalf("Load = (%q, %v)", data, err)
	}
}

func TestLoaderFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "a.yaml"), []byte("a: 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	loader := NewLoader(WithBaseDir(dir))
	data, err := loader.Load(context.Background(), Source{File: "a.yaml"})
	if err != nil || string(data) != "a: 1" {
		t.Fatalf("Load = (%q, %v)", data, err)
	}
}

func TestLoaderRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	loader := NewLoader(WithBaseDir(dir))
	if _, err := loader.Load(
		context.Background(), Source{File: "../outside"}); !errdefs.IsForbidden(err) {
		t.Fatalf("lexical escape error = %v, want forbidden", err)
	}

	outside := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(outside, "x"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := loader.Load(
		context.Background(), Source{File: "link/x"}); !errdefs.IsForbidden(err) {
		t.Fatalf("symlink escape error = %v, want forbidden", err)
	}
}

func TestLoaderMaxBytes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "big"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	loader := NewLoader(WithBaseDir(dir), WithMaxBytes(4))
	if _, err := loader.Load(
		context.Background(), Source{File: "big"}); !errdefs.IsValidation(err) {
		t.Fatalf("size cap error = %v, want validation", err)
	}
}

func TestLoaderEmbed(t *testing.T) {
	fsys := fstest.MapFS{
		"assets/a.yaml": &fstest.MapFile{Data: []byte("a: 1")},
	}
	loader := NewLoader(WithEmbed(fsys))
	data, err := loader.Load(
		context.Background(), Source{Embed: "assets/a.yaml"})
	if err != nil || string(data) != "a: 1" {
		t.Fatalf("Load = (%q, %v)", data, err)
	}
	if _, err := loader.Load(
		context.Background(), Source{Embed: "missing"}); !errdefs.IsNotFound(err) {
		t.Fatalf("missing embed error = %v, want not found", err)
	}
	if _, err := NewLoader().Load(
		context.Background(), Source{Embed: "x"}); !errdefs.IsValidation(err) {
		t.Fatalf("missing embed FS error = %v, want validation", err)
	}
}
