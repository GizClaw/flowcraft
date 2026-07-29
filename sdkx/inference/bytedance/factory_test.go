package bytedance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
)

func buildProvider(
	t *testing.T,
	spec map[string]any,
	profiles []config.ResolvedProfile,
) inference.ProviderDefinition {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := Factory().Build(context.Background(), config.ProviderInput{
		ID:       "bytedance",
		Spec:     raw,
		Profiles: profiles,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return provider
}

func testProfiles() []config.ResolvedProfile {
	secret, err := config.NewSecret([]byte("test-key"))
	if err != nil {
		panic(err)
	}
	return []config.ResolvedProfile{{
		ID:      "default",
		Secrets: map[string]config.Secret{SecretAPIKey: secret},
	}}
}

func speechProfiles() []config.ResolvedProfile {
	key, err := config.NewSecret([]byte("test-key"))
	if err != nil {
		panic(err)
	}
	spec, err := json.Marshal(ProfileSpec{AppID: "test-app"})
	if err != nil {
		panic(err)
	}
	return []config.ResolvedProfile{{
		ID:      "default",
		Secrets: map[string]config.Secret{SecretAPIKey: key},
		Spec:    spec,
	}}
}

func TestFactoryExposesCatalog(t *testing.T) {
	provider := buildProvider(t, map[string]any{}, testProfiles())
	if len(provider.Models) != len(catalog) {
		t.Fatalf("models = %d, want %d", len(provider.Models), len(catalog))
	}
	byName := make(map[string]inference.ModelImplementation, len(provider.Models))
	for _, model := range provider.Models {
		byName[model.Descriptor.ID.Name] = model
	}
	seed, ok := byName["doubao-seed-2-1-pro"]
	if !ok {
		t.Fatal("doubao-seed-2-1-pro missing from catalog")
	}
	if seed.Openers.Generate == nil {
		t.Fatal("generate model has no generate opener")
	}
	legacy := byName["doubao-seed-1-6"]
	if legacy.Descriptor.Lifecycle.Status != inference.ModelStatusDeprecated {
		t.Fatalf("seed-1-6 lifecycle = %q", legacy.Descriptor.Lifecycle.Status)
	}
	if legacy.Descriptor.Lifecycle.Replacement == nil ||
		legacy.Descriptor.Lifecycle.Replacement.Name != "doubao-seed-2-0-mini" {
		t.Fatalf("seed-1-6 replacement = %+v", legacy.Descriptor.Lifecycle.Replacement)
	}
	if byName["doubao-seedream-5-0"].Openers.Generate == nil {
		t.Fatal("image model has no generate opener")
	}
	if byName["doubao-embedding-large"].Openers.Embed == nil {
		t.Fatal("embed model has no embed opener")
	}
	if byName["doubao-asr-sauc-2-0"].Openers.Transcription == nil {
		t.Fatal("asr model has no transcription opener")
	}
	if byName["doubao-seeduplex-3-0"].Openers.Realtime == nil {
		t.Fatal("realtime model has no realtime opener")
	}
}

func TestFactorySpecValidation(t *testing.T) {
	cases := []struct {
		name string
		spec map[string]any
	}{
		{"unknown field", map[string]any{"bogus": true}},
		{"credential-shaped key", map[string]any{"api_key": "inline"}},
		{"bad base url", map[string]any{"base_url": "ftp://x"}},
		{"empty endpoint", map[string]any{"endpoints": map[string]string{"m": " "}}},
		{"duplicate model", map[string]any{"models": []map[string]any{
			{"name": "x", "kind": "generate"},
			{"name": "x", "kind": "embed"},
		}}},
		{"unknown kind", map[string]any{"models": []map[string]any{
			{"name": "x", "kind": "wat"},
		}}},
		{"bad model name", map[string]any{"models": []map[string]any{
			{"name": "bad name", "kind": "generate"},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Factory().Build(context.Background(), config.ProviderInput{
				ID:       "bytedance",
				Spec:     raw,
				Profiles: testProfiles(),
			})
			if err == nil {
				t.Fatal("expected Build to reject the spec")
			}
		})
	}
}

func TestFactorySecretValidation(t *testing.T) {
	bogus, err := config.NewSecret([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Factory().Build(context.Background(), config.ProviderInput{
		ID: "bytedance",
		Profiles: []config.ResolvedProfile{{
			ID:      "default",
			Secrets: map[string]config.Secret{"password": bogus},
		}},
	})
	if err == nil {
		t.Fatal("expected unknown secret names to be rejected")
	}

	_, err = Factory().Build(context.Background(), config.ProviderInput{
		ID: "bytedance",
		Profiles: []config.ResolvedProfile{{
			ID:      "default",
			Secrets: map[string]config.Secret{},
		}},
	})
	if err == nil {
		t.Fatal("expected profiles without any credential to be rejected")
	}
}

func TestFactoryCustomModelAndEndpoint(t *testing.T) {
	provider := buildProvider(t, map[string]any{
		"endpoints": map[string]string{
			"doubao-seed-2-1-pro": "ep-20260729-abcde",
		},
		"models": []map[string]any{{
			"name":   "my-internal-model",
			"kind":   "generate",
			"vision": true,
		}},
	}, testProfiles())
	var custom *inference.ModelImplementation
	for index := range provider.Models {
		if provider.Models[index].Descriptor.ID.Name == "my-internal-model" {
			custom = &provider.Models[index]
		}
	}
	if custom == nil {
		t.Fatal("custom model missing")
	}
	if custom.Openers.Generate == nil {
		t.Fatal("custom generate model has no opener")
	}
}

func TestFactoryBuildsRuntime(t *testing.T) {
	provider := buildProvider(t, map[string]any{}, speechProfiles())
	if _, err := inference.NewRuntime([]inference.ProviderDefinition{provider}); err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
}

func TestEndpointResolution(t *testing.T) {
	spec := Spec{
		Endpoints: map[string]string{"a": "ep-1"},
	}
	if got := spec.endpoint("a"); got != "ep-1" {
		t.Fatalf("endpoint(a) = %q", got)
	}
	if got := spec.endpoint("b"); got != "b" {
		t.Fatalf("endpoint(b) = %q", got)
	}
}
