package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// OpenAI's Chat Completions API implements automatic prompt caching on
// the **byte-exact prefix** of (system + tools + early-history). Hits
// are silent — the SDK reports them only via the `cached_tokens` field
// inside the usage block on the response — but the routing layer that
// decides whether a request lands on a backend node with the prior
// prefix already warmed lives behind the `prompt_cache_key` field.
// See https://platform.openai.com/docs/guides/prompt-caching for the
// canonical spec (TL;DR: passing a stable `prompt_cache_key` keeps
// requests with the same key on the same backend node, raising the
// implicit cache hit-rate from "round-robin lottery" to "deterministic
// hit when the prefix is identical").
//
// Strategy: the adapter computes a deterministic hash of the
// cache-eligible prefix at request build time (system messages plus
// tool definitions, both rendered canonically), truncates to 16 hex
// chars, and injects it as the `prompt_cache_key`. Caller code does
// not need to know about it — the existing convention "stable parts
// at the head, volatile parts at the tail" naturally produces a
// stable hash when the caller's prompt assembler is well-behaved.
//
// Why not include message history in the hash:
//   - Multi-turn conversations vary turn-by-turn; including history
//     would defeat the purpose (each call would land on a fresh
//     backend node).
//   - OpenAI's automatic prefix caching already covers the
//     identical-prefix-of-history case; the `prompt_cache_key` only
//     needs to anchor the stable-by-design parts.
//   - Limiting the hash to system + tools matches OpenAI's own
//     guidance and how the Python OpenAI client builds the key.

// computePromptCacheKey returns a deterministic 16-hex-char routing
// hint derived from the cache-eligible prefix of the request. Returns
// an empty string when there's nothing stable to hash (no system
// messages and no tools) — in that case the caller should omit the
// field rather than emit an empty value.
func computePromptCacheKey(msgs []llm.Message, tools []llm.ToolDefinition) string {
	h := sha256.New()
	// System segments come first, joined by a NUL separator so two
	// adjacent segments cannot collide with a single segment of the
	// concatenation (e.g. ["a","bc"] vs. ["ab","c"]).
	var hadStable bool
	for _, m := range msgs {
		if m.Role != llm.RoleSystem {
			continue
		}
		t := strings.TrimSpace(m.Content())
		if t == "" {
			continue
		}
		_, _ = h.Write([]byte(t))
		_, _ = h.Write([]byte{0})
		hadStable = true
	}
	// Tools: order-stable canonical JSON of (name, description,
	// input_schema) — the SDK preserves tool order from the caller
	// so we don't re-sort, but we sort the JSON keys of the schema
	// to absorb Go map iteration nondeterminism.
	if len(tools) > 0 {
		_, _ = h.Write([]byte{0xff})
		for _, td := range tools {
			_, _ = h.Write([]byte(td.Name))
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte(td.Description))
			_, _ = h.Write([]byte{0})
			if b, err := canonicalJSON(td.InputSchema); err == nil {
				_, _ = h.Write(b)
			}
			_, _ = h.Write([]byte{0})
		}
		hadStable = true
	}
	if !hadStable {
		return ""
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8]) // 16 hex chars
}

// canonicalJSON serialises v with sorted map keys at every level so
// the output is byte-stable across Go map iteration orders. This is
// the difference between "deterministic cache key for the same
// schema" and "fresh key every process restart".
func canonicalJSON(v any) ([]byte, error) {
	// Round-trip through encoding/json to get a generic
	// map/slice tree, then re-encode with our key-sorted walker.
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	var b strings.Builder
	writeCanonical(&b, tree)
	return []byte(b.String()), nil
}

func writeCanonical(b *strings.Builder, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			writeCanonical(b, t[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, item)
		}
		b.WriteByte(']')
	default:
		// numbers, strings, booleans, null — encoding/json handles
		// them deterministically without further help.
		enc, _ := json.Marshal(t)
		b.Write(enc)
	}
}
