package llm_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// cachedTokensProviders is the subset of conformance providers we
// exercise for prompt-cache hit-rate. Each entry must point at a
// model SKU that actually supports prefix caching on the relevant
// platform:
//
//   - Anthropic / Minimax → explicit cache_control set by the
//     adapter (sdkx/llm/anthropic/cache.go).
//   - Azure OpenAI / OpenAI / DeepSeek / Qwen-flash → implicit
//     automatic prefix caching once the prompt exceeds the
//     provider's minimum (≥ 1024 tokens on all four).
//   - ByteDance Doubao → transparent prefix caching, no opt-in.
//
// We deliberately leave Anthropic out of the main `providers`
// list in provider_test.go (it is gated behind FLOWCRAFT_TEST_ANTHROPIC
// and most CI hosts do not have an Anthropic key) but include it
// here because the cache normalisation (three-bucket → gross/cached)
// is unique to that family and worth covering end-to-end whenever
// the key is present.
var cachedTokensProviders = []providerSpec{
	{Provider: "anthropic", Env: "FLOWCRAFT_TEST_ANTHROPIC"},
	{Provider: "minimax", Env: "FLOWCRAFT_TEST_MINIMAX"},
	{Provider: "qwen", Env: "FLOWCRAFT_TEST_QWEN"},
	{Provider: "bytedance", Env: "FLOWCRAFT_TEST_BYTEDANCE"},
	{Provider: "azure", Env: "FLOWCRAFT_TEST_AZURE"},
	{Provider: "deepseek", Env: "FLOWCRAFT_TEST_DEEPSEEK"},
}

// buildStableSystemPrompt returns a deterministic ~22 KB string that
// safely clears every supported provider's minimum-cacheable-prompt
// threshold:
//
//   - Anthropic / Minimax: 1024-token minimum cache breakpoint (see
//     https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching#cache-limitations).
//     Below this the API silently refuses to honour `cache_control`.
//   - OpenAI / Azure OpenAI: prompt caching kicks in at 1024 prompt
//     tokens (https://platform.openai.com/docs/guides/prompt-caching).
//   - DeepSeek: hard 1024-token minimum.
//   - Qwen-flash: no documented floor but quieter regions need
//     long prompts to reliably populate the cache index.
//   - ByteDance Doubao: transparent prefix caching, no documented
//     floor; generous length still helps.
//
// We aim for ≥ 3000 tokens (≈ 22 KB English) to clear every floor
// with comfortable margin. The seed is a literal block (no random
// padding, no timestamps) so the prefix is byte-identical across
// calls — providers compute cache keys by exact-prefix hashing, so
// a single shifting byte would tank the hit rate to 0%.
func buildStableSystemPrompt() string {
	const seed = "You are a calm, precise assistant. Reply concisely and avoid speculation. " +
		"Treat every user message as a self-contained task with no implicit shared history. " +
		"Quote facts only when the supplied context contains them verbatim. " +
		"If the user request is ambiguous, ask exactly one short clarifying question and stop. " +
		"Do not invent tool names, file paths, URLs, dates, version numbers, or quantities. "
	return strings.Repeat(seed, 50) // ≈ 22 KB ≈ 4000+ tokens, comfortably above every provider's 1024 floor.
}

// TestProviders_PromptCacheHitRate verifies end-to-end that the
// prompt-cache plumbing introduced in sdkx/llm/{anthropic,openai,
// bytedance}/cache.go + the CachedInputTokens normalisation in the
// adapter Generate paths actually produces a non-zero cache hit
// rate on a second back-to-back call with an identical stable
// prefix.
//
// Why this lives in conformance (not adapter httptest): the value
// the user cares about is "did our cache_control / prompt_cache_key
// / multi-segment system convention land on the wire in a way the
// real provider actually rewarded with cheaper billing?". Replaying
// a hand-rolled JSON payload through httptest can only tell us the
// adapter *reads* `cached_tokens` correctly — it cannot tell us the
// upstream cache machinery decided we deserved a hit. Only a live
// double-call can.
//
// Skip semantics: each provider's sub-test calls t.Skip() when its
// env var is missing, so a credential-less `make conformance` run
// becomes a no-op for this file. This matches the existing
// conformance suite's convention.
func TestProviders_PromptCacheHitRate(t *testing.T) {
	stableSystem := buildStableSystemPrompt()

	for _, spec := range cachedTokensProviders {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)

			// Use a single generous timeout that covers both calls:
			// warm-up writes the cache (no observable saving), the
			// follow-up reads it. We sequence them under one ctx so
			// a hang on call #1 does not let call #2 leak past.
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			baseMsgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, stableSystem),
				llm.NewTextMessage(llm.RoleUser, "Step 1 of 2: respond with the literal word OK."),
			}

			// Warm-up call. Most providers will report
			// CachedInputTokens=0 here (cold cache). Anthropic +
			// ByteDance may already report a low non-zero value when
			// another recent run primed the same prefix — that's
			// fine; we treat this as advisory only.
			_, u1, err := provider.Generate(ctx, baseMsgs, llm.WithMaxTokens(20), llm.WithTemperature(0.1))
			if err != nil {
				t.Fatalf("warm-up call failed: %v", err)
			}
			t.Logf("warm-up: input=%d cached=%d output=%d (hit-rate=%.1f%%)",
				u1.InputTokens, u1.CachedInputTokens, u1.OutputTokens,
				ratePct(u1.CachedInputTokens, u1.InputTokens))

			// Follow-up call: identical system + tools, only the
			// trailing user message changes. The fresh user tokens
			// are short (<50 tokens) compared with the multi-KB
			// system prefix, so the cache hit rate must dominate.
			hitMsgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, stableSystem),
				llm.NewTextMessage(llm.RoleUser, "Step 2 of 2: respond with the literal word DONE."),
			}
			_, u2, err := provider.Generate(ctx, hitMsgs, llm.WithMaxTokens(20), llm.WithTemperature(0.1))
			if err != nil {
				t.Fatalf("follow-up call failed: %v", err)
			}
			t.Logf("follow-up: input=%d cached=%d output=%d (hit-rate=%.1f%%)",
				u2.InputTokens, u2.CachedInputTokens, u2.OutputTokens,
				ratePct(u2.CachedInputTokens, u2.InputTokens))

			// Hard contract: input must be reported and dominate
			// output. Without this the rest of the assertions are
			// meaningless (a zero-input call cannot have a cache
			// breakdown).
			if u2.InputTokens == 0 {
				t.Fatalf("InputTokens=0 on follow-up: provider didn't report prompt usage")
			}

			// Soft contract: most providers honour the cache. We
			// only enforce a STRICT non-zero requirement on the
			// adapters where we either explicitly drive caching
			// (anthropic / minimax via cache_control) or where
			// transparent caching is documented to be reliable for
			// the model SKU (bytedance Doubao). For OpenAI-family
			// implicit caching the threshold can be flaky on
			// quieter regions / smaller SKUs, so we only warn.
			strictHit := spec.Provider == "anthropic" || spec.Provider == "minimax" || spec.Provider == "bytedance"
			if strictHit && u2.CachedInputTokens == 0 {
				t.Errorf("expected CachedInputTokens>0 on follow-up for %s (cache writes via adapter cache_control / transparent prefix), got 0",
					spec.Provider)
			}
			if !strictHit && u2.CachedInputTokens == 0 {
				t.Logf("WARN: provider %s reported CachedInputTokens=0 on follow-up. Could be (a) cold region, (b) prompt below provider minimum, or (c) adapter regression. Investigate before claiming optimisation works.",
					spec.Provider)
			}

			// Invariant — same as the unit test, but enforced
			// against live wire numbers so an adapter regression
			// that decodes the wrong field would be caught here.
			if u2.CachedInputTokens > u2.InputTokens {
				t.Errorf("invariant violated: cached %d > input %d", u2.CachedInputTokens, u2.InputTokens)
			}

			// Anthropic-specific assertion: validate the
			// three-bucket → gross normalisation end-to-end. After
			// a warm follow-up the gross InputTokens must exceed
			// the new-tokens-only count (which would be small —
			// just the changed user line). We approximate this by
			// requiring InputTokens to be at least 200 tokens; on
			// any model SKU emitting only `input_tokens` (the
			// legacy bug the adapter fixed) this would collapse
			// to <100 tokens and fail.
			if spec.Provider == "anthropic" || spec.Provider == "minimax" {
				if u2.InputTokens < 200 {
					t.Errorf("anthropic-family InputTokens=%d on warm follow-up looks too small — normalizeAnthropicUsage may be summing only the non-cached bucket. Expected gross prompt size (system + user) ≥ 200 tokens.",
						u2.InputTokens)
				}
			}
		})
	}
}

// ratePct safely computes a percentage for logging without
// divide-by-zero noise. Returns 0 instead of NaN when denominator
// is zero (the "no prompt usage reported" case the assertions catch
// separately).
func ratePct(num, denom int64) float64 {
	if denom == 0 {
		return 0
	}
	return float64(num) / float64(denom) * 100
}
