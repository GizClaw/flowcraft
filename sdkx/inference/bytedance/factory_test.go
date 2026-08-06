package bytedance

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/config"
	"github.com/GizClaw/flowcraft/sdk/message"
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

func testProfilesWithSpec(t *testing.T, spec ProfileSpec) []config.ResolvedProfile {
	t.Helper()
	secret, err := config.NewSecret([]byte("test-key"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return []config.ResolvedProfile{{
		ID:      "default",
		Secrets: map[string]config.Secret{SecretAPIKey: secret},
		Spec:    raw,
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
	profiles := testProfilesWithSpec(t, ProfileSpec{
		Endpoints: map[string]string{"doubao-seed-2-1-pro": "ep-20260729-abcde"},
	})
	provider := buildProvider(t, map[string]any{
		"models": []map[string]any{{
			"name":   "my-internal-model",
			"kind":   "generate",
			"vision": true,
		}},
	}, profiles)
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
	cls := &clients{endpoints: map[string]string{"a": "ep-1"}}
	if got := cls.endpoint("a"); got != "ep-1" {
		t.Fatalf("endpoint(a) = %q", got)
	}
	if got := cls.endpoint("b"); got != "b" {
		t.Fatalf("endpoint(b) = %q", got)
	}
	if err := (ProfileSpec{Endpoints: map[string]string{"bad name": "ep-1"}}).Validate(); err == nil {
		t.Fatal("invalid endpoint model name accepted")
	}
	if err := (ProfileSpec{Endpoints: map[string]string{"m": " "}}).Validate(); err == nil {
		t.Fatal("empty endpoint accepted")
	}
}

func akskSecrets(t *testing.T) map[string]config.Secret {
	t.Helper()
	ak, err := config.NewSecret([]byte("test-ak"))
	if err != nil {
		t.Fatal(err)
	}
	sk, err := config.NewSecret([]byte("test-sk"))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]config.Secret{SecretAccessKey: ak, SecretSecretKey: sk}
}

func TestAKSKProfileMaterial(t *testing.T) {
	material, err := newProfileMaterial(config.ResolvedProfile{
		ID:      "default",
		Secrets: akskSecrets(t),
	})
	if err != nil {
		t.Fatalf("newProfileMaterial: %v", err)
	}
	if material.accessKey != "test-ak" || material.secretKey != "test-sk" {
		t.Fatalf("material = %+v", material)
	}
	cls := material.newClients(Spec{})
	if cls.ark == nil || cls.arkAuth != arkAuthAKSK {
		t.Fatalf("ark client = %+v", cls)
	}

	apiKey, err := config.NewSecret([]byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	mixed := akskSecrets(t)
	mixed[SecretAPIKey] = apiKey
	if _, err := newProfileMaterial(config.ResolvedProfile{
		ID: "default", Secrets: mixed,
	}); err == nil {
		t.Fatal("api_key mixed with AK/SK accepted")
	}

	for name, secrets := range map[string]map[string]config.Secret{
		"access key without secret key": {SecretAccessKey: akskSecrets(t)[SecretAccessKey]},
		"secret key without access key": {SecretSecretKey: akskSecrets(t)[SecretSecretKey]},
	} {
		if _, err := newProfileMaterial(config.ResolvedProfile{
			ID: "default", Secrets: secrets,
		}); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestAKSKProfileCannotOpenMediaGeneration(t *testing.T) {
	provider := buildProvider(t, map[string]any{}, []config.ResolvedProfile{{
		ID:      "default",
		Secrets: akskSecrets(t),
	}})
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{provider})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	imageRequest := inference.GenerateRequest{Input: inference.GenerateInput{
		Role: inference.InputRoleUser,
		Content: inference.InputContent{
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "a boat"},
			}},
			Intent: inference.Intent{Image: &inference.ImageIntent{}},
		},
	}}
	for _, model := range []string{"doubao-seedream-5-0", "doubao-seedance-2-0"} {
		_, err := runtime.Generate(context.Background(), generateModel(model), imageRequest)
		if err == nil {
			t.Fatalf("%s opened under AK/SK", model)
		}
		var chain strings.Builder
		for current := err; current != nil; current = errors.Unwrap(current) {
			chain.WriteString(current.Error())
			chain.WriteByte('|')
		}
		if !strings.Contains(chain.String(), "AK/SK") {
			t.Fatalf("%s error = %v, want AK/SK mention", model, err)
		}
	}
}
