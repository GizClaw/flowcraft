package resolver

import (
	"context"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

// resolveLLMs walks every LLMProfile, materialises its api key,
// and produces:
//
//   - the per-profile llm.LLM map handed to engine / probe
//     factories via Deps.LLMClients (the simple-path access)
//   - a profile-keyed llm.LLMResolver consumers can use for
//     credential-profile-aware lookup (rare in v0.1.0; provided
//     so future engine factories can opt in)
//
// Errors on construction are aggregated into errs so a single bad
// LLMProfile does not block the rest of the configuration from
// being checked.
func resolveLLMs(
	cat *catalog.Catalog,
	profiles map[string]v1alpha1.LLMProfile,
	lookup SecretLookup,
	opts ResolveOptions,
) (map[string]llm.LLM, llm.LLMResolver, *Errors) {
	errs := &Errors{}
	clients := make(map[string]llm.LLM, len(profiles))
	pcs := make(map[string]llm.ProviderConfig, len(profiles))

	for name, prof := range profiles {
		fn, err := cat.LLMProvider(prof.Spec.Provider)
		if err != nil {
			errs.add(err)
			continue
		}
		apiKey, err := resolveValueRef(prof.Spec.Auth.APIKey, lookup, opts, "LLMProfile "+name+".spec.auth.apiKey")
		if err != nil {
			errs.add(err)
			continue
		}
		pc, err := fn(name, prof.Spec.Config, apiKey)
		if err != nil {
			errs.add(err)
			continue
		}
		pcs[name] = pc

		// Skip live client construction when the caller asked
		// for validate-only mode (no IO). The Plan still carries
		// the resolved ProviderConfig so `vesseld plan` shows it.
		if !opts.AllowSecret && !opts.AllowFile {
			continue
		}
		// defaultModel is the model id we hand to llm.NewFromConfig
		// so the underlying provider can bind it. Empty is fine —
		// providers fall back to their own default.
		defaultModel, _ := pc.Config["default_model"].(string)
		client, err := llm.NewFromConfig(prof.Spec.Provider, defaultModel, pc.Config)
		if err != nil {
			errs.add(errdefs.Validationf("vesseld LLMProfile %q: provider construction: %v", name, err))
			continue
		}
		clients[name] = client
	}

	resolver := llm.DefaultResolver(&profileStore{configs: pcs})
	return clients, resolver, errs
}

// profileStore is a tiny llm.ProviderConfigStore backed by the
// resolved LLMProfile map. Lookup is by (provider, profile=name).
// We deliberately ignore the `provider` argument in Get because a
// profile name is unique across the config; the resolver passes
// both arguments anyway as part of the upstream contract.
type profileStore struct {
	configs map[string]llm.ProviderConfig
}

func (s *profileStore) GetProviderConfig(_ context.Context, _, profile string) (*llm.ProviderConfig, error) {
	pc, ok := s.configs[profile]
	if !ok {
		return nil, errdefs.NotFoundf("vesseld llm: profile %q not found", profile)
	}
	return &pc, nil
}
