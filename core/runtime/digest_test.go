package runtime

import "testing"

func digestConfig(secret string) Config {
	return Config{
		Version: 1,
		Sandbox: SandboxConfig{Backend: BackendLocal, Writes: []string{"/work"}},
		Files:   FilesConfig{Roots: []string{"/work"}},
		Inference: InferenceConfig{
			Providers: []ProviderConfig{{
				Name: "p", Driver: "openai",
				Settings: settingsJSON(map[string]any{"api_key": secret, "base_url": "https://x"}),
			}},
			Models: []ModelConfig{{Provider: "p", Model: "m"}},
		},
	}
}

// Rotating a credential must not invalidate every checkpoint, and the digest
// must not become a way to confirm a guessed secret.
func TestDigestIgnoresSecretValues(t *testing.T) {
	first := digestConfig("sk-first").Digest()
	second := digestConfig("sk-second").Digest()
	if first != second {
		t.Errorf("digest changed with the secret: %s != %s", first, second)
	}
	if first == "" {
		t.Fatal("digest is empty")
	}
}

func TestDigestTracksPermissions(t *testing.T) {
	base := digestConfig("k")
	changed := digestConfig("k")
	changed.Sandbox.Writes = []string{"/work", "/tmp"}
	if base.Digest() == changed.Digest() {
		t.Error("digest ignored a permission change")
	}
}

func TestDigestTracksTheModel(t *testing.T) {
	base := digestConfig("k")
	changed := digestConfig("k")
	changed.Inference.Models[0].Model = "other"
	if base.Digest() == changed.Digest() {
		t.Error("digest ignored a model change")
	}
}

// The digest is computed over a canonical form, so reading the same deployment
// twice yields the same value.
func TestDigestIsStable(t *testing.T) {
	cfg := digestConfig("k")
	cfg.Inference.Providers[0].Settings = settingsJSON(map[string]any{
		"z": "1", "a": "2", "m": "3",
	})
	first := cfg.Digest()
	cfg.Inference.Providers[0].Settings = settingsJSON(map[string]any{
		"a": "2", "m": "3", "z": "1",
	})
	if got := cfg.Digest(); got != first {
		t.Errorf("digest depends on map order: %s != %s", got, first)
	}
}
