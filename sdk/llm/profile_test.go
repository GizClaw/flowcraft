package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestCredentialProfile_RoundTrip(t *testing.T) {
	ctx := WithCredentialProfile(context.Background(), "tenant-a")
	if got := CredentialProfileFromContext(ctx); got != "tenant-a" {
		t.Fatalf("profile from ctx = %q, want %q", got, "tenant-a")
	}
}

func TestCredentialProfile_DefaultEmpty(t *testing.T) {
	if got := CredentialProfileFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty default, got %q", got)
	}
}

func TestCredentialProfile_NilContext_ReturnsEmpty(t *testing.T) {
	// Defensive: never panic on a nil ctx (cheap, predictable behavior
	// is more valuable than catching the nil-ctx misuse here).
	if got := CredentialProfileFromContext(nil); got != "" {
		t.Fatalf("nil-ctx should return empty, got %q", got)
	}
}

func TestSimpleProviderConfigStore_DefaultProfileOnly(t *testing.T) {
	called := 0
	store := &SimpleProviderConfigStore{
		Lookup: func(_ context.Context, provider string) (*ProviderConfig, error) {
			called++
			return &ProviderConfig{Provider: provider, Config: map[string]any{"k": "v"}}, nil
		},
	}
	pc, err := store.GetProviderConfig(context.Background(), "openai", "")
	if err != nil {
		t.Fatal(err)
	}
	if pc.Provider != "openai" {
		t.Errorf("provider mismatch: %q", pc.Provider)
	}
	if called != 1 {
		t.Errorf("Lookup called %d times, want 1", called)
	}
}

func TestSimpleProviderConfigStore_NonEmptyProfile_NotFound(t *testing.T) {
	store := &SimpleProviderConfigStore{
		Lookup: func(_ context.Context, _ string) (*ProviderConfig, error) {
			t.Fatal("Lookup should NOT be called when profile != ''")
			return nil, nil
		},
	}
	_, err := store.GetProviderConfig(context.Background(), "openai", "tenant-a")
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("expected NotFound error, got %v", err)
	}
}

func TestSimpleProviderConfigStore_NoLookup_NotFound(t *testing.T) {
	store := &SimpleProviderConfigStore{}
	_, err := store.GetProviderConfig(context.Background(), "openai", "")
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("expected NotFound for missing Lookup, got %v", err)
	}
}

func TestSimpleProviderConfigStore_LookupErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom")
	store := &SimpleProviderConfigStore{
		Lookup: func(_ context.Context, _ string) (*ProviderConfig, error) {
			return nil, wantErr
		},
	}
	_, err := store.GetProviderConfig(context.Background(), "openai", "")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wantErr propagated, got %v", err)
	}
}
