package resource

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestSecretSchemeDefaultStore(t *testing.T) {
	stores := map[string]SecretStore{
		"kc": SecretStoreFunc{
			LookupFn: func(_ context.Context, name string) (string, bool, error) {
				if name == "api" {
					return "secret-value", true, nil
				}
				return "", false, nil
			},
			DefaultSecret: true,
		},
	}
	out, err := Expand(context.Background(),
		[]byte(`{"root": "${secret:api}"}`),
		WithResolver(NewResolver(NewSecretScheme(stores, "kc"))))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if string(out) != `{"root":"secret-value"}` {
		t.Fatalf("Expand = %s", out)
	}
}

func TestSecretSchemeNamedStore(t *testing.T) {
	stores := map[string]SecretStore{
		"env": SecretStoreFunc{
			LookupFn: func(_ context.Context, name string) (string, bool, error) {
				if name == "other" {
					return "env-value", true, nil
				}
				return "", false, nil
			},
		},
	}
	out, err := Expand(context.Background(),
		[]byte(`{"root": "${secret:env.other}"}`),
		WithResolver(NewResolver(NewSecretScheme(stores, ""))))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if string(out) != `{"root":"env-value"}` {
		t.Fatalf("Expand = %s", out)
	}
}

func TestSecretSchemeErrors(t *testing.T) {
	boom := errors.New("store down")
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
			name:         "missing secret",
			ref:          "${secret:missing}",
			defaultStore: "env",
			stores: map[string]SecretStore{
				"env": SecretStoreFunc{LookupFn: func(context.Context, string) (string, bool, error) {
					return "", false, nil
				}},
			},
		},
		{
			name:         "store error",
			ref:          "${secret:down.api}",
			defaultStore: "",
			stores: map[string]SecretStore{
				"down": SecretStoreFunc{LookupFn: func(context.Context, string) (string, bool, error) {
					return "", false, boom
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
			_, err := Expand(context.Background(),
				[]byte(`{"a": "`+tc.ref+`"}`),
				WithResolver(NewResolver(NewSecretScheme(tc.stores, tc.defaultStore))))
			if !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}
}

func TestSecretSchemeEscaped(t *testing.T) {
	out, err := Expand(context.Background(),
		[]byte(`{"a": "\\${secret:api}"}`),
		WithResolver(NewResolver(NewSecretScheme(nil, ""))))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if string(out) != `{"a":"${secret:api}"}` {
		t.Fatalf("Expand = %s, want escaped literal", out)
	}
}
