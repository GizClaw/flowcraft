package secret

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestFileStoreRegisters(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "file"); !ok {
		t.Fatalf("factory %s/file missing", ResourceKind)
	}
}

func TestFileStoreReadsSecretFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok-456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := fileFactory{}.New(context.Background(), resource.Input{
		Settings: []byte(`{"base": "` + dir + `", "id": "files", "default": true}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store := value.(resource.SecretStore)
	if m, ok := value.(resource.SecretStoreID); !ok || m.SecretStoreID() != "files" {
		t.Fatalf("SecretStoreID = %v, want files", m)
	}
	if !store.DefaultSecretStore() {
		t.Fatal("DefaultSecretStore = false, want true")
	}
	got, found, err := store.Lookup(context.Background(), "token")
	if err != nil || !found || got != "tok-456" {
		t.Fatalf("Lookup = (%q, %v, %v), want tok-456 (trailing newline stripped)", got, found, err)
	}
}

func TestFileStoreMissingAndEscape(t *testing.T) {
	dir := t.TempDir()
	value, err := fileFactory{}.New(context.Background(), resource.Input{
		Settings: []byte(`{"base": "` + dir + `"}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store := value.(resource.SecretStore)
	if _, found, err := store.Lookup(context.Background(), "missing"); err != nil || found {
		t.Fatalf("missing Lookup = (%v, %v), want not found", found, err)
	}
	if _, found, err := store.Lookup(context.Background(), "../secret"); err == nil || found {
		t.Fatalf("escape Lookup = (%v, %v), want error", found, err)
	}
}

func TestFileStoreValidation(t *testing.T) {
	_, err := fileFactory{}.New(context.Background(), resource.Input{
		Settings: []byte(`{"default": true}`),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("missing base error = %v, want validation", err)
	}
	_, err = fileFactory{}.New(context.Background(), resource.Input{
		Settings: []byte(`{"base": "/tmp", "bogus": 1}`),
	})
	if err == nil {
		t.Fatal("New accepted unknown settings field")
	}
}
