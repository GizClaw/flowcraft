package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	oai "github.com/openai/openai-go"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// ComputePromptCacheKey returns a deterministic 16-hex-char routing hint
// derived from stable request inputs.
func ComputePromptCacheKey(msgs []llm.Message, tools []llm.ToolDefinition) string {
	h := sha256.New()
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
	return hex.EncodeToString(sum[:8])
}

func canonicalJSON(v any) ([]byte, error) {
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
		enc, _ := json.Marshal(t)
		b.Write(enc)
	}
}

// CachedInputTokensFromUsage extracts the cached-input token count from a
// chat-completion usage block. It prefers the standard OpenAI field
// usage.prompt_tokens_details.cached_tokens; if that is zero it falls back to
// the DeepSeek-compatible top-level field prompt_cache_hit_tokens, which some
// OpenAI-compatible providers (notably DeepSeek) emit instead.
func CachedInputTokensFromUsage(usage oai.CompletionUsage) int64 {
	if cached := usage.PromptTokensDetails.CachedTokens; cached != 0 {
		return cached
	}
	raw := usage.RawJSON()
	if raw == "" {
		return 0
	}
	var body struct {
		PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		return 0
	}
	return body.PromptCacheHitTokens
}
