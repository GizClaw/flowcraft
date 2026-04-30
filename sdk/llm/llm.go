// Package llm is FlowCraft's provider-agnostic LLM facade. It defines
// a small interface ([LLM]) every provider implements, a registry
// that maps "provider/model" strings to concrete instances
// ([ProviderRegistry], [DefaultRegistry]), and a [LLMResolver] that
// turns deployment configuration into ready-to-call LLM values with
// model-specific behavior already wired in.
//
// # Layered architecture
//
// The package is intentionally split along two axes — data vs.
// behavior, and per-model-fixed vs. per-call:
//
//	┌─────────────────── core data types (no behavior) ───────────────────┐
//	│  llm.go           [LLM] / [StreamMessage] interfaces                │
//	│  option.go        [GenerateOptions] + With… per-call options        │
//	│  capability.go    [Capability] enum + [ModelCaps] black-list        │
//	│  spec.go          [ModelSpec] = Caps + Defaults + Limits, mergeSpec │
//	│  aliases.go       re-exports of sdk/model (Message, ToolCall, …)    │
//	└─────────────────────────────────────────────────────────────────────┘
//	┌────────── runtime composition (turns data into behavior) ───────────┐
//	│  with_defaults.go [WithDefaults] — fill nil opts from spec defaults │
//	│  with_caps.go     [WithCaps]     — drop / downgrade unsupported,    │
//	│                                    fold system messages, downgrade  │
//	│                                    streaming, validate modalities   │
//	│  with_limits.go   [WithLimits]   — clamp numeric fields to ceilings │
//	└─────────────────────────────────────────────────────────────────────┘
//	┌──────────────── orchestration (catalog + lookup) ───────────────────┐
//	│  factory.go       [ProviderRegistry] + [ProviderFactory] + catalog  │
//	│  resolver.go      [LLMResolver] = config + cache + assembly         │
//	│  profile.go       credential profile ctx helpers                    │
//	│  fallback.go      [FallbackLLM] for provider redundancy             │
//	└─────────────────────────────────────────────────────────────────────┘
//
// # Quick start (single-credential deployment)
//
//	store := &llm.SimpleProviderConfigStore{
//	    Lookup: func(_ context.Context, provider string) (*llm.ProviderConfig, error) {
//	        return &llm.ProviderConfig{
//	            Provider: provider,
//	            Config:   map[string]any{"api_key": os.Getenv("OPENAI_API_KEY")},
//	        }, nil
//	    },
//	}
//	resolver := llm.DefaultResolver(store)
//	model, err := resolver.Resolve(ctx, "openai/gpt-4o")
//	msg, usage, err := model.Generate(ctx, []llm.Message{
//	    llm.NewTextMessage(llm.RoleUser, "hi"),
//	})
//
// # Spec — what the model is, in one type
//
// [ModelSpec] groups everything the runtime needs to know about a
// model that does not change call-to-call:
//
//   - Caps   — capabilities the model does NOT support (black-list).
//   - Defaults — default GenerateOptions field values, applied when
//     the caller leaves a field nil.
//   - Limits — numeric hard ceilings the runtime clamps to (with a
//     telemetry warning), e.g. MaxOutputTokens.
//
// Spec is layered: catalog declaration ⊕ ProviderConfig.SpecOverride
// ⊕ ModelConfig.SpecOverride. Field-wise merge; non-zero overlays
// win, and Limits use stricter-wins (deployment overrides can only
// tighten the catalog). See [mergeSpec].
//
// # Three-layer middleware (defaults → caps → limits)
//
// At resolve time the resolver wraps the raw provider with three
// middleware in a fixed outer-to-inner order. Per-call invocation
// flows through them as defaults → caps → limits → provider:
//
//  1. [WithDefaults] runs first so the rest of the chain sees a
//     "complete" request. Caller-set fields always win; only nil
//     pointers / empty slices get filled.
//  2. [WithCaps] runs second, after the request is complete. It
//     drops or downgrades fields the model claims not to support
//     (Temperature → nil, JSONSchema → JSONMode, system messages
//     folded into the first user message, GenerateStream downgraded
//     to one-chunk Generate, etc.) and rejects requests with
//     unsupported modality parts (vision / audio / file) before any
//     network call.
//  3. [WithLimits] runs last, clamping numeric fields the model does
//     support but to within its hard limits. Running before caps
//     would waste work clamping fields that get stripped anyway.
//
// Each wrapper short-circuits to a no-op when its input is the zero
// value, so the chain has no dummy structs.
//
// # Credential profile (multi-tenant routing)
//
// A single provider can carry multiple credential profiles —
// per-tenant API keys, key-pool entries, prod-vs-staging splits.
// The dimension is opt-in and zero-impact for callers that don't
// use it:
//
//   - Tag a context with [WithCredentialProfile] at the call boundary
//     (HTTP middleware, pod boot, agent spec wiring).
//   - The resolver reads it via [CredentialProfileFromContext] and
//     looks up the matching ProviderConfig via
//     [ProviderConfigStore.GetProviderConfig]. Strict match — a
//     missing profile errors instead of silently routing to the
//     default credential.
//   - The resolver cache and [LLMResolver.InvalidateCache] both key
//     on (provider, model, profile), so per-tenant eviction is
//     possible via [WithProfile].
//
// # Compatibility
//
// New code should consume [WithCaps] / [WithDefaults] / [WithLimits]
// directly, [ProviderRegistry.LookupModelSpec] for catalog lookups,
// and [WithPolicyCaps] for resolver-wide policy. The pre-redesign
// names — [CapsMiddleware], [WithExtraCaps],
// [ProviderRegistry.LookupModelCaps], [ModelInfo.Caps] — are kept
// as thin shims in deprecated.go and on the deprecation field, all
// scheduled for removal in v0.3.0.
//
// See doc/sdk-llm-redesign.md for the full design rationale,
// per-cap behavior table, and migration guide.
package llm

import "context"

// LLM is the unified interface for language model interactions.
type LLM interface {
	Generate(ctx context.Context, messages []Message, opts ...GenerateOption) (Message, TokenUsage, error)
	GenerateStream(ctx context.Context, messages []Message, opts ...GenerateOption) (StreamMessage, error)
}

// StreamMessage is an iterator over streaming response chunks.
// Call Next() in a loop; Current() returns the latest chunk.
// After iteration completes, Message() and Usage() return the
// accumulated result.
type StreamMessage interface {
	Next() bool
	Current() StreamChunk
	Err() error
	Close() error
	Message() Message
	Usage() Usage
}
