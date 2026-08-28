package resource

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestSecretSchemeDefaultStore(t *testing.T) {
	resolver := NewSecretResolver(map[string]SecretStore{
		"kc": SecretStoreFunc{
			LookupFn: func(_ context.Context, name string) (string, bool, error) {
				if name == "api" {
					return "secret-value", true, nil
				}
				return "", false, nil
			},
			DefaultSecret: true,
		},
	}, "kc")
	out, err := Expand(context.Background(),
		[]byte(`{"root": "${secret:api}"}`),
		WithResolver(NewResolver(resolver.Scheme())))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if string(out) != `{"root":{"store":"kc","name":"api"}}` {
		t.Fatalf("Expand = %s, want lazy ref marker", out)
	}
	got, err := DecodeTyped[struct {
		Root Secret `json:"root"`
	}](context.Background(), out)
	if err != nil {
		t.Fatalf("DecodeTyped: %v", err)
	}
	if !got.Root.IsRef() {
		t.Fatal("Root is not a ref")
	}
	if value, err := got.Root.Resolve(context.Background(), resolver); err != nil || value != "secret-value" {
		t.Fatalf("Resolve = (%q, %v), want secret-value", value, err)
	}
}

func TestSecretSchemeNamedStore(t *testing.T) {
	resolver := NewSecretResolver(map[string]SecretStore{
		"env": SecretStoreFunc{
			LookupFn: func(_ context.Context, name string) (string, bool, error) {
				if name == "other" {
					return "env-value", true, nil
				}
				return "", false, nil
			},
		},
	}, "")
	out, err := Expand(context.Background(),
		[]byte(`{"root": "${secret:env.other}"}`),
		WithResolver(NewResolver(resolver.Scheme())))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if string(out) != `{"root":{"store":"env","name":"other"}}` {
		t.Fatalf("Expand = %s, want named store ref marker", out)
	}
}

func TestSecretExpansionErrors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ref          string
		defaultStore string
		stores       map[string]SecretStore
	}{
		{
			name:         "no default store",
			ref:          "${secret:api}",
			defaultStore: "",
			stores:       map[string]SecretStore{},
		},
		{
			name:         "unknown store",
			ref:          "${secret:nope.api}",
			defaultStore: "",
			stores: map[string]SecretStore{
				"env": SecretStoreFunc{LookupFn: func(context.Context, string) (string, bool, error) {
					return "", false, nil
				}},
			},
		},
		{
			name:         "empty secret name",
			ref:          "${secret:env.}",
			defaultStore: "",
			stores: map[string]SecretStore{
				"env": SecretStoreFunc{LookupFn: func(context.Context, string) (string, bool, error) {
					return "", false, nil
				}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := NewSecretResolver(tc.stores, tc.defaultStore)
			_, err := Expand(context.Background(),
				[]byte(`{"a": "`+tc.ref+`"}`),
				WithResolver(NewResolver(resolver.Scheme())))
			if !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}
}

func TestSecretResolveErrors(t *testing.T) {
	boom := errors.New("store down")
	resolver := NewSecretResolver(map[string]SecretStore{
		"down": SecretStoreFunc{LookupFn: func(context.Context, string) (string, bool, error) {
			return "", false, boom
		}},
		"empty": SecretStoreFunc{LookupFn: func(context.Context, string) (string, bool, error) {
			return "", false, nil
		}},
	}, "")
	for _, tc := range []struct {
		name  string
		ref   SecretRef
		other *SecretResolver
	}{
		{name: "missing secret", ref: SecretRef{Store: "empty", Name: "nope"}},
		{name: "store error", ref: SecretRef{Store: "down", Name: "x"}},
		{name: "unknown store", ref: SecretRef{Store: "ghost", Name: "x"}},
		{name: "nil resolver", ref: SecretRef{Store: "empty", Name: "x"}, other: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret := Secret{ref: &tc.ref}
			r := resolver
			if tc.other != nil {
				r = tc.other
			}
			if _, err := secret.Resolve(context.Background(), r); !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}
}

func TestSecretRedaction(t *testing.T) {
	secret := LiteralSecret("super-secret")
	if secret.String() != "<secret>" {
		t.Fatalf("String = %q, want <secret>", secret.String())
	}
	raw, err := secret.MarshalJSON()
	if err != nil || string(raw) != `"<secret>"` {
		t.Fatalf("MarshalJSON = %s, %v; want <secret>", raw, err)
	}
	if secret.IsRef() {
		t.Fatal("literal secret reports IsRef")
	}
	if value, err := secret.Resolve(context.Background(), nil); err != nil || value != "super-secret" {
		t.Fatalf("literal Resolve = (%q, %v)", value, err)
	}
}

func TestSecretDecodeRejectsBadShape(t *testing.T) {
	if _, err := DecodeTyped[struct {
		Root Secret `json:"root"`
	}](context.Background(), []byte(`{"root": 42}`)); !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want validation", err)
	}
}

func TestSecretSchemeEscaped(t *testing.T) {
	resolver := NewSecretResolver(nil, "")
	out, err := Expand(context.Background(),
		[]byte(`{"a": "\\${secret:api}"}`),
		WithResolver(NewResolver(resolver.Scheme())))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if string(out) != `{"a":"${secret:api}"}` {
		t.Fatalf("Expand = %s, want escaped literal", out)
	}
}

func TestCachingSecretStoreDisabled(t *testing.T) {
	var calls int
	inner := SecretStoreFunc{
		LookupFn: func(_ context.Context, name string) (string, bool, error) {
			calls++
			return "value", true, nil
		},
		DefaultSecret: true,
	}
	cached := NewCachingSecretStore(inner, 0) // ttl 0 disables cache
	if _, found, err := cached.Lookup(context.Background(), "x"); err != nil || !found {
		t.Fatalf("Lookup = (%v, %v)", found, err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 with caching disabled", calls)
	}
	if !cached.DefaultSecretStore() {
		t.Fatal("DefaultSecretStore = false, want true")
	}
}

func TestCachingSecretStoreHits(t *testing.T) {
	var calls int
	inner := SecretStoreFunc{
		LookupFn: func(_ context.Context, name string) (string, bool, error) {
			calls++
			return "value", true, nil
		},
	}
	cached := NewCachingSecretStore(inner, time.Hour)
	for i := 0; i < 3; i++ {
		if _, found, err := cached.Lookup(context.Background(), "x"); err != nil || !found {
			t.Fatalf("Lookup %d = (%v, %v)", i, found, err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 with cache hits", calls)
	}
}
