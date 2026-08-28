package secret

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
)

func TestEnvFactoryRegisters(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "env"); !ok {
		t.Fatalf("factory %s/env missing", ResourceKind)
	}
}

func TestEnvStoreLooksUpEnvironment(t *testing.T) {
	t.Setenv("SECRET_TEST_TOKEN", "tok-123")
	value, err := envFactory{}.New(context.Background(), resource.Input{
		Settings: []byte(`{"default": true}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store, ok := value.(resource.SecretStore)
	if !ok {
		t.Fatalf("value = %T, want resource.SecretStore", value)
	}
	if !store.DefaultSecretStore() {
		t.Fatal("DefaultSecretStore = false, want true")
	}
	got, found, err := store.Lookup(context.Background(), "SECRET_TEST_TOKEN")
	if err != nil || !found || got != "tok-123" {
		t.Fatalf("Lookup = (%q, %v, %v), want tok-123", got, found, err)
	}
	if _, found, err := store.Lookup(context.Background(), "SECRET_TEST_MISSING"); err != nil || found {
		t.Fatalf("missing secret Lookup = (%v, %v), want not found", found, err)
	}
}

func TestEnvStoreStrictDecode(t *testing.T) {
	value, err := envFactory{}.New(context.Background(), resource.Input{
		Settings: []byte(`{"default": true, "bogus": 1}`),
	})
	if err == nil {
		t.Fatal("New accepted unknown settings field")
	}
	if value != nil {
		t.Fatalf("New returned %v despite error", value)
	}
}
