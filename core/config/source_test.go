package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// Bytes is the cheapest source and the one a generated or embedded document
// uses when it is a constant of the binary.
func TestBytesSource(t *testing.T) {
	body, err := Bytes([]byte("name: forge\n")).Body(context.Background())
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if string(body) != "name: forge\n" {
		t.Errorf("body = %q", body)
	}
}

func TestFileSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.yaml")
	if err := os.WriteFile(path, []byte("name: forge\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, err := File(path).Body(context.Background())
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if string(body) != "name: forge\n" {
		t.Errorf("body = %q", body)
	}
}

// FS is what makes an embedded tree a layer source: embed.FS satisfies io/fs,
// and so does a DirFS over a directory the binary sits beside.
func TestFSSource(t *testing.T) {
	fsys := fstest.MapFS{
		"config/base.yaml": {Data: []byte("name: forge\n")},
	}
	body, err := FS(fsys, "config/base.yaml").Body(context.Background())
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if string(body) != "name: forge\n" {
		t.Errorf("body = %q", body)
	}
	if _, err := FS(fsys, "config/absent.yaml").Body(context.Background()); err == nil {
		t.Fatal("a missing name was accepted")
	}
}

// Func is the seam for a body that is not a file: fetched, decrypted, templated
// or generated at load time.
func TestFuncSource(t *testing.T) {
	calls := 0
	body, err := Func(func(context.Context) ([]byte, error) {
		calls++
		return []byte("name: generated\n"), nil
	}).Body(context.Background())
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if calls != 1 || string(body) != "name: generated\n" {
		t.Errorf("calls = %d, body = %q", calls, body)
	}

	boom := errors.New("vault is down")
	_, err = Func(func(context.Context) ([]byte, error) { return nil, boom }).Body(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the source's own error", err)
	}

	var missing func(context.Context) ([]byte, error)
	if _, err := Func(missing).Body(context.Background()); err == nil {
		t.Fatal("a nil function was accepted")
	}
}

// A source failure is reported with the layer it came from: "read failed" alone
// sends the reader to the wrong file.
func TestSourceErrorNamesTheLayer(t *testing.T) {
	boom := errors.New("vault is down")
	_, _, err := load(t, Layer{
		Priority: 0,
		Name:     "secrets.yaml",
		Source:   Func(func(context.Context) ([]byte, error) { return nil, boom }),
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the source's error", err)
	}
	if !strings.Contains(err.Error(), "secrets.yaml") {
		t.Errorf("err = %v, want the layer named", err)
	}
}
