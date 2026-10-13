package typesafe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// TestResourceFactoryBuildsProviderWithEnvSecret pins the deployment path end to
// end: one settings document expands an ${env:...} credential, builds two
// decision models, and hands back a provider definition whose models carry
// decision openers.
func TestResourceFactoryBuildsProviderWithEnvSecret(t *testing.T) {
	t.Setenv("TYPESAFE_TEST_KEY", "sk-test")
	settings, err := resource.Expand(context.Background(),
		json.RawMessage(`{
			"id": "typesafe",
			"spec": {"models": [
				{"name": "jev-latest", "kind": "decision", "capabilities": {"inputs": ["text"]}},
				{"name": "jev-1.13.0", "kind": "decision", "capabilities": {"inputs": ["text"]}}
			]},
			"profiles": [{
				"id": "default",
				"operations": ["decision"],
				"secrets": {"api_key": "${env:TYPESAFE_TEST_KEY}"}
			}]
		}`), resource.ExpandEnv())
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	value, err := Factory().New(context.Background(), resource.Input{Settings: settings})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider, ok := value.(inference.ProviderDefinition)
	if !ok {
		t.Fatalf("New returned %T, want inference.ProviderDefinition", value)
	}
	if provider.ID != "typesafe" || len(provider.Profiles) != 1 ||
		provider.Profiles[0].ID != "default" || len(provider.Models) != 2 {
		t.Fatalf("provider = %+v", provider)
	}
	// The declarations come back in name order, so a document that lists the
	// same models differently still builds the same definition.
	if provider.Models[0].Descriptor.ID.Name != "jev-1.13.0" ||
		provider.Models[1].Descriptor.ID.Name != "jev-latest" {
		t.Fatalf("model order = %q, %q",
			provider.Models[0].Descriptor.ID.Name,
			provider.Models[1].Descriptor.ID.Name,
		)
	}
	for _, declared := range provider.Models {
		if declared.Descriptor.ID.Provider != "typesafe" {
			t.Errorf("model provider = %q", declared.Descriptor.ID.Provider)
		}
		if declared.Openers.Decision == nil {
			t.Errorf("model %q has no decision opener", declared.Descriptor.ID.Name)
		}
	}
	// The credential is a lazy reference, so it is only read when a profile
	// opens a driver — and the opener is where a missing key surfaces.
	driver, err := provider.Models[0].Openers.Decision(
		context.Background(), typesafeModel("jev-1.13.0"))
	if err != nil {
		t.Fatalf("open decision driver: %v", err)
	}
	if driver == nil {
		t.Fatal("opener returned no driver")
	}
}

// TestResourceSpecHTTPRetriesFromEnv pins that http_retries is a deployment
// knob, readable from the environment like every other scalar setting.
func TestResourceSpecHTTPRetriesFromEnv(t *testing.T) {
	t.Setenv("TYPESAFE_TEST_HTTP_RETRIES", "4")
	settings, err := resource.DecodeTyped[ResourceSettings](
		context.Background(),
		json.RawMessage(`{
			"id": "typesafe",
			"spec": {"http_retries": "${env:TYPESAFE_TEST_HTTP_RETRIES}"}
		}`),
		resource.ExpandEnv(),
	)
	if err != nil {
		t.Fatalf("DecodeTyped: %v", err)
	}
	spec, err := decodeSpec(context.Background(), settings.Spec)
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if spec.HTTPRetries == nil || int(*spec.HTTPRetries) != 4 {
		t.Fatalf("HTTPRetries = %v, want 4", spec.HTTPRetries)
	}
}

// TestResourceFactoryRejectsBadSettings pins that the settings document fails
// closed: a key this driver does not implement, a credential it does not know,
// a transport fact it owns itself, or a model declaration outside the route's
// contract all fail the build rather than being ignored.
func TestResourceFactoryRejectsBadSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		want     string
	}{
		{
			name:     "missing id",
			settings: `{"profiles":[]}`,
			want:     "settings.id is required",
		},
		{
			name:     "unknown top-level key",
			settings: `{"id":"typesafe","mode":"fast","profiles":[]}`,
			want:     `unknown field "mode"`,
		},
		{
			name:     "unknown spec key",
			settings: `{"id":"typesafe","spec":{"stream":true},"profiles":[]}`,
			want:     `unknown field "stream"`,
		},
		{
			name: "unknown profile secret",
			settings: `{"id":"typesafe","profiles":[{"id":"default",
				"secrets":{"token":"x"}}]}`,
			want: `unknown secret "token"`,
		},
		{
			name:     "profile without an id",
			settings: `{"id":"typesafe","profiles":[{"secrets":{}}]}`,
			want:     "missing its id",
		},
		{
			name:     "profile spec this driver does not implement",
			settings: `{"id":"typesafe","profiles":[{"id":"default","spec":{"region":"eu"}}]}`,
			want:     `unknown field "region"`,
		},
		{
			name:     "negative http_retries",
			settings: `{"id":"typesafe","spec":{"http_retries":-1},"profiles":[]}`,
			want:     "http_retries must not be negative",
		},
		{
			name:     "base url without a scheme",
			settings: `{"id":"typesafe","spec":{"endpoint":{"base_url":"api.typesafe.ai/v1"}},"profiles":[]}`,
			want:     "must be an http(s) URL",
		},
		{
			name:     "static header that is not a header name",
			settings: `{"id":"typesafe","spec":{"endpoint":{"headers":{"X Tenant":"acme"}}},"profiles":[]}`,
			want:     "is not a header name",
		},
		{
			name:     "static header the transport owns",
			settings: `{"id":"typesafe","spec":{"endpoint":{"headers":{"Authorization":"Bearer x"}}},"profiles":[]}`,
			want:     "is set by the driver for every request",
		},
		{
			name:     "duplicate model declaration",
			settings: `{"id":"typesafe","spec":{"models":[{"name":"jev-latest","kind":"decision"},{"name":"jev-latest","kind":"decision"}]},"profiles":[]}`,
			want:     "duplicate model declaration",
		},
		{
			name:     "model name outside the alias grammar",
			settings: `{"id":"typesafe","spec":{"models":[{"name":"jev latest","kind":"decision"}]},"profiles":[]}`,
			want:     "invalid model name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newProvider(t, tc.settings)
			if err == nil {
				t.Fatal("New accepted settings the driver cannot honour")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestResourceFactoryRequiresNoCredentialAtBuildTime pins lazy resolution: a
// profile with no api_key still builds, because the key is read when a driver
// opens — the failure belongs to the call that needs the credential, not to
// every deployment that declares the provider.
func TestResourceFactoryRequiresNoCredentialAtBuildTime(t *testing.T) {
	if _, err := newProvider(t, `{
		"id": "typesafe",
		"profiles": [{"id": "default"}]
	}`); err != nil {
		t.Fatalf("New rejected a profile without api_key: %v", err)
	}
}

// TestRegisterAddsTypeSafeProviderFactory pins the registry entry a deployment
// resolves by kind and impl.
func TestRegisterAddsTypeSafeProviderFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, providerID); !ok {
		t.Fatalf("factory %s/%s missing", ResourceKind, providerID)
	}
}
