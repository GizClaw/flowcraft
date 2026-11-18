package sources_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
	"github.com/goccy/go-json"
)

// open runs a kind the way the loader does: the settings blob in, a layer out.
func open(t *testing.T, kind deploy.SourceKind, settings string) (deploy.Source, error) {
	t.Helper()
	return kind.Open(context.Background(), json.RawMessage(settings))
}

// read is open plus the body, which is what a test about bytes usually wants.
func read(t *testing.T, kind deploy.SourceKind, settings string) (deploy.Source, string, error) {
	t.Helper()
	layer, err := open(t, kind, settings)
	if err != nil {
		return deploy.Source{}, "", err
	}
	body, err := layer.Read(context.Background())
	return layer, string(body), err
}

func TestFileReadsAPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatalf("write layer: %v", err)
	}
	layer, body, err := read(t, sources.File(), `{"path":"`+path+`"}`)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	// The layer is named by its path: the reference already is a location, and
	// that is what a reader needs to see.
	if layer.Name != path {
		t.Errorf("name = %q, want the path", layer.Name)
	}
	if body != "version: 1\n" {
		t.Errorf("body = %q", body)
	}
}

// A file source says where it looks: a missing path and a typo are both refused
// at load, before any layer is merged.
func TestFileRejectsBadSettings(t *testing.T) {
	for name, settings := range map[string]string{
		"no path":       `{}`,
		"blank path":    `{"path":"  "}`,
		"unknown key":   `{"path":"/x","bogus":1}`,
		"not an object": `"runtime.yaml"`,
	} {
		if _, err := open(t, sources.File(), settings); err == nil {
			t.Errorf("%s: a bad settings block was accepted", name)
		}
	}
}

// Inline is the document carrying its own bytes: no file, no host value.
func TestInlineCarriesTheBody(t *testing.T) {
	layer, body, err := read(t, sources.Inline(), `{"body":"version: 1\nname: base\n"}`)
	if err != nil {
		t.Fatalf("Inline: %v", err)
	}
	if body != "version: 1\nname: base\n" {
		t.Errorf("body = %q", body)
	}
	// Two inline layers in one stack would otherwise be indistinguishable in an
	// error, so the default name is short and the document can override it.
	if layer.Name != "inline" {
		t.Errorf("name = %q, want the default", layer.Name)
	}
	named, err := open(t, sources.Inline(), `{"name":"base","body":"version: 1\n"}`)
	if err != nil {
		t.Fatalf("Inline: %v", err)
	}
	if named.Name != "base" {
		t.Errorf("name = %q, want the one the document wrote", named.Name)
	}
}

func TestInlineRejectsBadSettings(t *testing.T) {
	for name, settings := range map[string]string{
		"no body":     `{}`,
		"blank body":  `{"body":"  \n"}`,
		"unknown key": `{"body":"version: 1\n","bogus":1}`,
	} {
		if _, err := open(t, sources.Inline(), settings); err == nil {
			t.Errorf("%s: a bad settings block was accepted", name)
		}
	}
}

// FS is the //go:embed case: the tree is the host's, the document names a member.
func TestFSReadsAMember(t *testing.T) {
	tree := fstest.MapFS{"runtime/base.yaml": {Data: []byte("version: 1\n")}}
	layer, body, err := read(t, sources.FS("assets", tree), `{"name":"runtime/base.yaml"}`)
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	if body != "version: 1\n" {
		t.Errorf("body = %q", body)
	}
	// The name carries the kind, because one process may carry several trees.
	if layer.Name != "assets:runtime/base.yaml" {
		t.Errorf("name = %q, want the kind and the member", layer.Name)
	}
}

func TestFSRejectsAMissingMember(t *testing.T) {
	tree := fstest.MapFS{"runtime/base.yaml": {Data: []byte("version: 1\n")}}
	layer, err := open(t, sources.FS("assets", tree), `{"name":"runtime/absent.yaml"}`)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := layer.Read(context.Background()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestFSRejectsBadRegistration(t *testing.T) {
	if _, err := open(t, sources.FS("assets", nil), `{"name":"x"}`); err == nil {
		t.Error("a source without a filesystem was accepted")
	}
	for name, settings := range map[string]string{
		"no name":     `{}`,
		"unknown key": `{"name":"x","bogus":1}`,
	} {
		if _, err := open(t, sources.FS("assets", fstest.MapFS{}), settings); err == nil {
			t.Errorf("%s: a bad settings block was accepted", name)
		}
	}
}

// Func is everything else: the document supplies a reference, the host supplies
// the loader and therefore every policy around it.
func TestFuncCallsTheLoaderWithTheReference(t *testing.T) {
	var got string
	kind := sources.Func("vault", func(_ context.Context, ref string) ([]byte, error) {
		got = ref
		return []byte("version: 1\n"), nil
	})
	layer, body, err := read(t, kind, `{"name":"openai/luna"}`)
	if err != nil {
		t.Fatalf("Func: %v", err)
	}
	if got != "openai/luna" {
		t.Errorf("loader saw %q, want the reference", got)
	}
	if body != "version: 1\n" {
		t.Errorf("body = %q", body)
	}
	if layer.Name != "vault:openai/luna" {
		t.Errorf("name = %q, want the kind and the reference", layer.Name)
	}
}

func TestFuncPassesTheLoadError(t *testing.T) {
	kind := sources.Func("vault", func(context.Context, string) ([]byte, error) {
		return nil, errors.New("the vault is sealed")
	})
	layer, err := open(t, kind, `{"name":"x"}`)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := layer.Read(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "sealed") {
		t.Fatalf("err = %v, want the loader's error", err)
	}
}

func TestFuncRejectsBadRegistration(t *testing.T) {
	if _, err := open(t, sources.Func("vault", nil), `{"name":"x"}`); err == nil {
		t.Error("a source without a loader was accepted")
	}
	for name, settings := range map[string]string{
		"no name":     `{}`,
		"unknown key": `{"name":"x","bogus":1}`,
	} {
		load := func(context.Context, string) ([]byte, error) { return nil, nil }
		if _, err := open(t, sources.Func("vault", load), settings); err == nil {
			t.Errorf("%s: a bad settings block was accepted", name)
		}
	}
}
