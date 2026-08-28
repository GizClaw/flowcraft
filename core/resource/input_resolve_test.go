package resource

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInputResolveSettingsYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "s.yaml"),
		[]byte("root: /tmp/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := Input{
		Settings: []byte(`{"file": "s.yaml"}`),
		Loader:   NewLoader(WithBaseDir(dir)),
	}
	data, err := in.ResolveSettings(context.Background())
	if err != nil {
		t.Fatalf("ResolveSettings: %v", err)
	}
	var settings struct {
		Root string `json:"root"`
	}
	if err := DecodeSettings(context.Background(), &settings, data); err != nil {
		t.Fatalf("DecodeSettings: %v", err)
	}
	if settings.Root != "/tmp/x" {
		t.Fatalf("root = %q, want /tmp/x", settings.Root)
	}
}

func TestInputResolveSettingsInline(t *testing.T) {
	in := Input{Settings: []byte(`{"root": "/tmp/x"}`)}
	data, err := in.ResolveSettings(context.Background())
	if err != nil {
		t.Fatalf("ResolveSettings: %v", err)
	}
	if string(data) != `{"root": "/tmp/x"}` {
		t.Fatalf("data = %s", data)
	}
}
