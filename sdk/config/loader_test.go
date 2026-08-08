package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestLoader_Literal(t *testing.T) {
	l := NewLoader()
	data, err := l.Load(context.Background(), Source{kind: SourceLiteral, literal: "version: v1"})
	if err != nil {
		t.Fatalf("load literal: %v", err)
	}
	if string(data) != "version: v1" {
		t.Fatalf("literal = %q", data)
	}
}

func TestLoader_StructuredContent(t *testing.T) {
	l := NewLoader()
	data, err := l.Load(context.Background(), Source{
		kind: SourceContent,
		raw:  json.RawMessage(`{"version":"v1","workspaces":{}}`),
	})
	if err != nil {
		t.Fatalf("load content: %v", err)
	}
	if string(data) != `{"version":"v1","workspaces":{}}` {
		t.Fatalf("content = %q", data)
	}
}

func TestLoader_EmptyContentRejected(t *testing.T) {
	l := NewLoader()
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`)} {
		_, err := l.Load(context.Background(), Source{kind: SourceContent, raw: raw})
		if !errdefs.IsValidation(err) {
			t.Fatalf("empty content %q error = %v, want validation", raw, err)
		}
	}
}

func TestLoader_EmptyLiteralRejected(t *testing.T) {
	l := NewLoader()
	_, err := l.Load(context.Background(), Source{})
	if !errdefs.IsValidation(err) {
		t.Fatalf("empty source error = %v, want validation", err)
	}
	_, err = l.Load(context.Background(), Source{kind: SourceLiteral, literal: ""})
	if !errdefs.IsValidation(err) {
		t.Fatalf("empty literal error = %v, want validation", err)
	}
}

func TestLoader_LiteralSizeBound(t *testing.T) {
	l := NewLoader(WithMaxBytes(4))
	_, err := l.Load(context.Background(), Source{kind: SourceLiteral, literal: "12345"})
	if !errdefs.IsValidation(err) {
		t.Fatalf("oversized literal error = %v, want validation", err)
	}
}

func TestLoader_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.yaml")
	if err := os.WriteFile(path, []byte("version: v1"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	l := NewLoader(WithBaseDir(dir))
	data, err := l.Load(context.Background(), Source{kind: SourceFile, path: "./doc.yaml"})
	if err != nil {
		t.Fatalf("load file: %v", err)
	}
	if string(data) != "version: v1" {
		t.Fatalf("file = %q", data)
	}
}

func TestLoader_FileNotFound(t *testing.T) {
	l := NewLoader(WithBaseDir(t.TempDir()))
	_, err := l.Load(context.Background(), Source{kind: SourceFile, path: "./missing.yaml"})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("missing file error = %v, want not found", err)
	}
}

func TestLoader_FileEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.yaml")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	l := NewLoader(WithBaseDir(dir))
	for _, ref := range []string{
		"../secret.yaml",
		filepath.Join("..", "secret.yaml"),
		outside, // absolute path outside baseDir
	} {
		_, err := l.Load(context.Background(), Source{kind: SourceFile, path: ref})
		if !errdefs.IsForbidden(err) {
			t.Fatalf("escape %q error = %v, want forbidden", ref, err)
		}
	}
}

func TestLoader_FileSymlinkEscapeRejected(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "" {
		t.Skip("symlink creation may be restricted in CI sandbox")
	}
	base := t.TempDir()
	outside := filepath.Join(t.TempDir(), "target.yaml")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	link := filepath.Join(base, "link.yaml")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	l := NewLoader(WithBaseDir(base))
	_, err := l.Load(context.Background(), Source{kind: SourceFile, path: "./link.yaml"})
	if !errdefs.IsForbidden(err) {
		t.Fatalf("symlink escape error = %v, want forbidden", err)
	}
}

func TestLoader_FileDirectoryRejected(t *testing.T) {
	l := NewLoader(WithBaseDir(t.TempDir()))
	_, err := l.Load(context.Background(), Source{kind: SourceFile, path: "."})
	if !errdefs.IsValidation(err) {
		t.Fatalf("directory read error = %v, want validation", err)
	}
}

func TestLoader_FileSizeBound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(path, []byte("1234567890"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	l := NewLoader(WithBaseDir(dir), WithMaxBytes(4))
	_, err := l.Load(context.Background(), Source{kind: SourceFile, path: "./big.yaml"})
	if !errdefs.IsValidation(err) {
		t.Fatalf("oversized file error = %v, want validation", err)
	}
}

func TestLoader_Embed(t *testing.T) {
	fsys := fstest.MapFS{
		"scenarios/werewolf/deploy.yaml": {Data: []byte("version: v1")},
	}
	l := NewLoader(WithEmbed(fsys))
	data, err := l.Load(context.Background(), Source{kind: SourceEmbed, path: "scenarios/werewolf/deploy.yaml"})
	if err != nil {
		t.Fatalf("load embed: %v", err)
	}
	if string(data) != "version: v1" {
		t.Fatalf("embed = %q", data)
	}
}

func TestLoader_EmbedNotConfigured(t *testing.T) {
	l := NewLoader()
	_, err := l.Load(context.Background(), Source{kind: SourceEmbed, path: "a.yaml"})
	if !errdefs.IsValidation(err) {
		t.Fatalf("unconfigured embed error = %v, want validation", err)
	}
}

func TestLoader_EmbedNotFound(t *testing.T) {
	l := NewLoader(WithEmbed(fstest.MapFS{"a.yaml": {Data: []byte("x")}}))
	_, err := l.Load(context.Background(), Source{kind: SourceEmbed, path: "missing.yaml"})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("missing embed error = %v, want not found", err)
	}
}

func TestLoader_EmbedInvalidPath(t *testing.T) {
	l := NewLoader(WithEmbed(fstest.MapFS{}))
	for _, name := range []string{"../a.yaml", "/abs/a.yaml", ""} {
		_, err := l.Load(context.Background(), Source{kind: SourceEmbed, path: name})
		if !errdefs.IsValidation(err) {
			t.Fatalf("invalid embed path %q error = %v, want validation", name, err)
		}
	}
}

func TestLoader_RefResolves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recipe.yaml")
	if err := os.WriteFile(path, []byte("version: v1"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	l := NewLoader(WithBaseDir(dir))
	data, err := l.Resolve(context.Background(), Ref{kind: SourceFile, path: "./recipe.yaml"})
	if err != nil {
		t.Fatalf("resolve file ref: %v", err)
	}
	if string(data) != "version: v1" {
		t.Fatalf("ref = %q", data)
	}
}

func TestLoader_ContextCancelled(t *testing.T) {
	l := NewLoader()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Load(ctx, Source{kind: SourceLiteral, literal: "x"}); err == nil {
		t.Fatal("cancelled context accepted")
	}
}

func TestLoader_InstancesAreIsolated(t *testing.T) {
	a := NewLoader(WithEmbed(fstest.MapFS{"only-a.yaml": {Data: []byte("a")}}))
	b := NewLoader(WithEmbed(fstest.MapFS{"only-b.yaml": {Data: []byte("b")}}))
	if _, err := a.Load(context.Background(), Source{kind: SourceEmbed, path: "only-b.yaml"}); !errdefs.IsNotFound(err) {
		t.Fatalf("loader a saw b's embed, error = %v", err)
	}
	if _, err := b.Load(context.Background(), Source{kind: SourceEmbed, path: "only-a.yaml"}); !errdefs.IsNotFound(err) {
		t.Fatalf("loader b saw a's embed, error = %v", err)
	}
}

func TestInput_ResolveDocument(t *testing.T) {
	var o Opaque
	if err := json.Unmarshal([]byte(`{"version":"v1"}`), &o); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	in := Input{Settings: json.RawMessage(o), Resolve: NewLoader().Load}
	data, err := in.ResolveDocument(context.Background())
	if err != nil {
		t.Fatalf("ResolveDocument: %v", err)
	}
	if string(data) != `{"version":"v1"}` {
		t.Fatalf("document = %q", data)
	}
}

func TestInput_ResolveSourceNil(t *testing.T) {
	var in Input
	_, err := in.ResolveSource(context.Background(), Source{kind: SourceLiteral, literal: "x"})
	if !errdefs.IsValidation(err) {
		t.Fatalf("nil resolve error = %v, want validation", err)
	}
}

func TestInput_ResolveSource(t *testing.T) {
	var in Input
	in.Resolve = func(ctx context.Context, src Source) ([]byte, error) {
		return []byte("resolved"), nil
	}
	data, err := in.ResolveSource(context.Background(), Source{kind: SourceLiteral, literal: "x"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(data) != "resolved" {
		t.Fatalf("data = %q", data)
	}
}
