package llm

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Credential profile routing. See doc/sdk-llm-redesign.md §3.4.
//
// "Profile" is a free-form string identifying a credential profile
// within a single provider — e.g. "tenant-a", "pool-3", "staging".
// SDK only defines the plumbing: who decides which profile a call
// uses is the call-boundary code's job (HTTP middleware, pod boot,
// agent spec wiring). 99% of callers never set a profile and see
// no behavior change.

type ctxKey struct{ name string }

var credentialProfileCtxKey = ctxKey{"llm.credential_profile"}

// WithCredentialProfile returns ctx tagged with the given credential
// profile. The resolver reads this tag when looking up
// ProviderConfig — empty string is the default profile.
//
// Typical placements:
//
//   - HTTP middleware: read tenant from header, tag ctx
//     before calling downstream handlers.
//   - Pod boot: pod sets its tenant once when starting a Run; every
//     LLM call inside the pod inherits the same profile.
//   - Agent / graph node spec: a declarative profile field is
//     translated into WithCredentialProfile by the runtime.
//   - Pool wrappers: a custom LLMResolver decorator picks a profile
//     per call via round-robin / random / weighted policy and tags
//     ctx before delegating.
//
// Setting profile == "" explicitly is equivalent to not calling
// this function at all (the default profile is selected).
func WithCredentialProfile(ctx context.Context, profile string) context.Context {
	return context.WithValue(ctx, credentialProfileCtxKey, profile)
}

// CredentialProfileFromContext returns the profile installed by
// WithCredentialProfile, or "" if none. Used internally by the
// resolver; safe to call from external code that wants to inspect
// the routing decision.
func CredentialProfileFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(credentialProfileCtxKey).(string)
	return v
}

// SimpleProviderConfigStore wraps a single-credential lookup
// (profile=="") in a ProviderConfigStore for the common
// single-credential deployment. Use a custom store implementation
// when multiple profiles per provider are needed.
//
// Calls with a non-empty profile return errdefs.NotFound — by
// construction this store has no notion of profiles, so a request
// for a specific one is a misconfiguration that should fail loud.
type SimpleProviderConfigStore struct {
	// Lookup returns the single ProviderConfig for the given provider.
	// Returning errdefs.NotFound is propagated as-is to the resolver;
	// any other error fails Resolve.
	Lookup func(ctx context.Context, provider string) (*ProviderConfig, error)
}

// GetProviderConfig implements ProviderConfigStore.
func (s *SimpleProviderConfigStore) GetProviderConfig(ctx context.Context, provider, profile string) (*ProviderConfig, error) {
	if profile != "" {
		return nil, errdefs.NotFoundf("llm: simple store has no profile %q for provider %q", profile, provider)
	}
	if s.Lookup == nil {
		return nil, errdefs.NotFoundf("llm: simple store has no Lookup configured")
	}
	return s.Lookup(ctx, provider)
}
