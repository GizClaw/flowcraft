package hostmcp

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

type fakeSecrets struct{}

func (fakeSecrets) Get(context.Context, string, string) (string, error) {
	return "value", nil
}
func (fakeSecrets) Set(context.Context, string, string, string) error { return nil }
func (fakeSecrets) Delete(context.Context, string, string) error      { return nil }

type fakeContext struct{}

func (fakeContext) Workspace(context.Context) (map[string]any, error) {
	return map[string]any{"path": "/ws"}, nil
}

func TestStandardRegistryAndHostAbout(t *testing.T) {
	registry := NewRegistry()
	if err := Standard(registry, Services{
		Secrets: fakeSecrets{},
		Context: fakeContext{},
	}, "0.1.0"); err != nil {
		t.Fatalf("Standard: %v", err)
	}
	if _, ok := registry.Lookup("secret_get"); !ok {
		t.Fatal("secret_get not registered")
	}
	if _, ok := registry.Lookup("inference_upsert"); ok {
		t.Fatal("inference_upsert registered without a service")
	}
	about, ok := registry.Lookup("host_about")
	if !ok {
		t.Fatal("host_about not registered")
	}
	value, err := about.Handler(context.Background(), Call{
		Identity: Identity{PluginID: "hello", Grants: GrantSet{}},
	})
	if err != nil {
		t.Fatalf("host_about: %v", err)
	}
	report, ok := value.(About)
	if !ok {
		t.Fatalf("host_about returned %T", value)
	}
	if report.Protocol != ProtocolVersion || report.HostVersion != "0.1.0" {
		t.Fatalf("report = %+v", report)
	}
	if !contains(report.Primitives, "secret_get") ||
		!contains(report.Primitives, "workspace_current") ||
		!contains(report.Primitives, "host_about") {
		t.Fatalf("primitives = %v", report.Primitives)
	}
	if !hasDenied(report.Denied, "inference_upsert", "no_service") {
		t.Fatalf("denied = %v, want inference_upsert/no_service", report.Denied)
	}
	if !hasDenied(report.Denied, "secret_get", "no_grant") {
		t.Fatalf("denied = %v, want secret_get/no_grant", report.Denied)
	}
}

func TestTokens(t *testing.T) {
	tokens := NewTokens()
	secret, token, err := tokens.Mint(Identity{
		PluginID: "hello",
		Grants:   GrantSet{"secrets:auth": true},
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// The store keeps digests, not the tokens themselves: nothing in the
	// process holds a string that could be replayed.
	if len(tokens.byHash) != 1 {
		t.Fatalf("token store holds %d entries, want 1", len(tokens.byHash))
	}
	for key := range tokens.byHash {
		if key == tokenOf(secret) {
			// The key is the digest of the secret, which is the point:
			// the secret itself is not a key.
			continue
		}
		t.Fatalf("the token store keys %v, want the digest of the minted secret", key)
	}
	if token.String() == "" || strings.Contains(token.String(), secret) {
		t.Fatalf("Token.String() = %q, want a digest prefix", token.String())
	}
	if token.IsZero() {
		t.Fatal("a minted Token reports IsZero")
	}
	if !(Token{}).IsZero() {
		t.Fatal("the zero Token does not report IsZero")
	}
	identity, minted, ok := tokens.Lookup(secret)
	if !ok || identity.PluginID != "hello" || !identity.Grants.Has("secrets:auth") {
		t.Fatalf("Lookup = %+v/%v", identity, ok)
	}
	if minted != token {
		t.Fatalf("Lookup token = %v, want the minted %v", minted, token)
	}
	tokens.Revoke(token)
	if _, _, ok := tokens.Lookup(secret); ok {
		t.Fatal("token still valid after Revoke")
	}
	// A token store that was never handed a token cannot be talked into
	// revoking one by a zero value, and revoking twice is a no-op.
	tokens.Revoke(Token{})
	tokens.Revoke(token)
	if len(tokens.byHash) != 0 {
		t.Fatalf("token store holds %d entries after Revoke, want 0", len(tokens.byHash))
	}
}

func TestStandardFilteredDisablesPrimitive(t *testing.T) {
	registry := NewRegistry()
	if err := StandardFiltered(registry, Services{Secrets: fakeSecrets{}},
		"0.1.0", []string{"secret_get"}); err != nil {
		t.Fatalf("StandardFiltered: %v", err)
	}
	if _, ok := registry.Lookup("secret_get"); ok {
		t.Fatal("disabled secret_get is exposed")
	}
	about, _ := registry.Lookup("host_about")
	value, err := about.Handler(context.Background(), Call{
		Identity: Identity{PluginID: "hello"},
	})
	if err != nil {
		t.Fatalf("host_about: %v", err)
	}
	if !hasDenied(value.(About).Denied, "secret_get", "disabled") {
		t.Fatalf("denied = %v, want secret_get/disabled", value.(About).Denied)
	}
}

// TestStandardFilteredRefusesToDisableHostAbout pins the one name the
// disabled list cannot carry: host_about is how a plugin learns which
// primitives exist, and reporting the disabled ones is its answer.
// Ignoring the entry would leave the application believing a primitive
// it named is hidden while every plugin still reaches it.
func TestStandardFilteredRefusesToDisableHostAbout(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	err := StandardFiltered(registry, Services{}, "0.1.0", []string{"host_about"})
	if !errdefs.IsValidation(err) {
		t.Fatalf("StandardFiltered with host_about disabled = %v, want a validation error",
			err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasDenied(entries []DeniedEntry, tool, reason string) bool {
	for _, entry := range entries {
		if entry.Tool == tool && entry.Reason == reason {
			return true
		}
	}
	return false
}
