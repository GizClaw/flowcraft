package resource

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func TestSecretSchemeDottedNames(t *testing.T) {
	resolver := NewSecretResolver(map[string]SecretStore{
		"env": SecretStoreFunc{LookupFn: func(_ context.Context, name string) (string, bool, error) {
			if name == "my.app.key" {
				return "dotted", true, nil
			}
			return "", false, nil
		}},
	}, "env")
	// A dotted NAME-only ref whose prefix is not a configured store
	// falls back to the default store with the whole ref as the name.
	out, err := Expand(context.Background(),
		[]byte(`{"root": "${secret:my.app.key}"}`),
		WithResolver(NewResolver(resolver.Scheme())))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if string(out) != `{"root":{"store":"env","name":"my.app.key"}}` {
		t.Fatalf("Expand = %s, want default-store dotted name", out)
	}
	// An explicit store keeps its prefix even with dotted names.
	out, err = Expand(context.Background(),
		[]byte(`{"root": "${secret:env.my.app.key}"}`),
		WithResolver(NewResolver(resolver.Scheme())))
	if err != nil {
		t.Fatalf("Expand explicit: %v", err)
	}
	if string(out) != `{"root":{"store":"env","name":"my.app.key"}}` {
		t.Fatalf("Expand explicit = %s, want named-store dotted name", out)
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

func TestCachingSecretStoreNegativeCache(t *testing.T) {
	var calls int
	inner := SecretStoreFunc{
		LookupFn: func(_ context.Context, name string) (string, bool, error) {
			calls++
			return "", false, nil
		},
	}
	cached := NewCachingSecretStore(inner, time.Hour)
	for i := 0; i < 3; i++ {
		if _, found, err := cached.Lookup(context.Background(), "missing"); err != nil || found {
			t.Fatalf("Lookup %d = (%v, %v), want not found", i, found, err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 with negative cache hits", calls)
	}
}

func TestCachingSecretStoreExpiry(t *testing.T) {
	var calls int
	inner := SecretStoreFunc{
		LookupFn: func(_ context.Context, name string) (string, bool, error) {
			calls++
			return "value", true, nil
		},
	}
	cached := NewCachingSecretStore(inner, 10*time.Millisecond)
	if _, found, err := cached.Lookup(context.Background(), "x"); err != nil || !found {
		t.Fatalf("Lookup = (%v, %v)", found, err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, found, err := cached.Lookup(context.Background(), "x"); err != nil || !found {
		t.Fatalf("Lookup after TTL = (%v, %v)", found, err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 after TTL expiry", calls)
	}
}

func TestCachingSecretStoreNilInnerErrors(t *testing.T) {
	var cached *CachingSecretStore
	if _, found, err := cached.Lookup(context.Background(), "x"); err == nil || found {
		t.Fatalf("nil store Lookup = (%v, %v), want error", found, err)
	}
	cached = NewCachingSecretStore(nil, time.Minute)
	if _, found, err := cached.Lookup(context.Background(), "x"); err == nil || found {
		t.Fatalf("nil inner Lookup = (%v, %v), want error", found, err)
	}
}

// TestCachingSecretStoreCapacityBounded is the leak regression test:
// resolving more distinct names than the cache holds must not grow it.
// Names are deployment data, so an unbounded map here retains one entry
// (and one secret value) per name ever resolved.
func TestCachingSecretStoreCapacityBounded(t *testing.T) {
	inner := SecretStoreFunc{
		LookupFn: func(_ context.Context, name string) (string, bool, error) {
			return "value-" + name, true, nil
		},
	}
	cached := NewCachingSecretStore(inner, time.Hour, WithCacheCapacity(2))
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if _, found, err := cached.Lookup(context.Background(), name); err != nil || !found {
			t.Fatalf("Lookup(%s) = (%v, %v)", name, found, err)
		}
	}
	cached.mu.Lock()
	size := len(cached.cache)
	cached.mu.Unlock()
	if size != 2 {
		t.Fatalf("cache holds %d entries, want the configured 2", size)
	}
}

// TestCachingSecretStoreEvictsSoonestExpiry pins which entry yields when
// the bound is reached: the one closest to expiry, so a long-lived name
// is not evicted in favour of one about to lapse.
func TestCachingSecretStoreEvictsSoonestExpiry(t *testing.T) {
	inner := SecretStoreFunc{
		LookupFn: func(_ context.Context, name string) (string, bool, error) {
			return "value-" + name, true, nil
		},
	}
	cached := NewCachingSecretStore(inner, time.Hour, WithCacheCapacity(2))
	if _, _, err := cached.Lookup(context.Background(), "short"); err != nil {
		t.Fatalf("Lookup(short): %v", err)
	}
	// Age the entry so "short" is unambiguously closest to expiry.
	cached.mu.Lock()
	entry := cached.cache["short"]
	entry.expires = entry.expires.Add(-30 * time.Minute)
	cached.cache["short"] = entry
	cached.mu.Unlock()

	for _, name := range []string{"long-a", "long-b"} {
		if _, _, err := cached.Lookup(context.Background(), name); err != nil {
			t.Fatalf("Lookup(%s): %v", name, err)
		}
	}

	cached.mu.Lock()
	defer cached.mu.Unlock()
	if _, ok := cached.cache["short"]; ok {
		t.Fatalf("cache kept the about-to-expire entry: %v", cached.cache)
	}
	if len(cached.cache) != 2 {
		t.Fatalf("cache holds %d entries, want 2", len(cached.cache))
	}
}

// TestCachingSecretStoreReclaimsExpiredEntries is the retention test:
// the TTL bounds how long a value is held, so an expired entry must be
// gone even when nothing looks that name up again — the previous form
// only overwrote an entry on the next lookup of the same name.
func TestCachingSecretStoreReclaimsExpiredEntries(t *testing.T) {
	inner := SecretStoreFunc{
		LookupFn: func(_ context.Context, name string) (string, bool, error) {
			return "value-" + name, true, nil
		},
	}
	cached := NewCachingSecretStore(inner, 5*time.Millisecond)
	if _, _, err := cached.Lookup(context.Background(), "old"); err != nil {
		t.Fatalf("Lookup(old): %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, _, err := cached.Lookup(context.Background(), "new"); err != nil {
		t.Fatalf("Lookup(new): %v", err)
	}

	cached.mu.Lock()
	defer cached.mu.Unlock()
	if _, ok := cached.cache["old"]; ok {
		t.Fatalf("expired entry outlived its TTL: %v", cached.cache)
	}
	if len(cached.cache) != 1 {
		t.Fatalf("cache holds %d entries, want 1", len(cached.cache))
	}
}

// TestSecretNeverRendersItsValue proves every formatting verb that can
// reach a log, an error or a crash report prints the marker — including
// %#v, which bypasses String() because it prints the struct itself.
func TestSecretNeverRendersItsValue(t *testing.T) {
	const value = "sk-live-do-not-print"
	secret := LiteralSecret(value)

	for _, verb := range []string{"%v", "%+v", "%s", "%q", "%#v"} {
		rendered := fmt.Sprintf(verb, secret)
		if rendered != "<secret>" && rendered != `"<secret>"` {
			t.Errorf("%s secret = %s, want the redaction marker", verb, rendered)
		}
		if strings.Contains(rendered, value) {
			t.Errorf("%s leaked the secret value: %s", verb, rendered)
		}
	}

	// The same must hold when a Secret is a field of a larger struct
	// that a caller prints wholesale.
	type holder struct {
		Name   string
		Secret Secret
	}
	rendered := fmt.Sprintf("%+v", holder{Name: "api", Secret: secret})
	if strings.Contains(rendered, value) {
		t.Fatalf("%%+v leaked the nested secret: %s", rendered)
	}
	if rendered != "{Name:api Secret:<secret>}" {
		t.Fatalf("nested render = %s, want the redaction marker", rendered)
	}
}
