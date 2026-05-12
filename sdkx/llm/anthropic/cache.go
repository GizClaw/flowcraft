package anthropic

import (
	"encoding/json"

	asdk "github.com/anthropics/anthropic-sdk-go"
)

// Anthropic prompt caching lets callers insert up to 4 `cache_control:
// {type: ephemeral}` breakpoints across system blocks, tool definitions,
// and message content. Each breakpoint creates a cacheable prefix
// ending at that block; on a subsequent call the API matches the
// longest identical prefix among all breakpoints and reuses the
// cached prompt at 10% read cost (vs. 25% write cost on first miss).
// See https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching.
//
// The adapter places breakpoints automatically using the conventions
// agreed in design discussion:
//
//   - Caller signals segment boundaries by emitting multiple
//     llm.Message{Role: System, ...} entries — each becomes a separate
//     Anthropic text block (no string join). Stable / long content
//     belongs in its own segment; short / volatile content (timestamps,
//     current state) belongs in its own segment.
//   - Long segments (≥ anthropicCacheMinChars characters) are anchor
//     candidates. Short segments are skipped — Anthropic's 1024-token
//     minimum cache size makes anchoring them a guaranteed 25%-write /
//     0%-read deal.
//   - Total breakpoints capped at anthropicMaxCacheBreakpoints (4).
//     Priority order when over budget: tools-end > history >
//     system-last > earlier-system-segments. Excess earliest anchors
//     are dropped; Anthropic's longest-prefix-wins matching means
//     losing the front loses only the shortest fallback cache.
const (
	// anthropicCacheMinChars approximates Anthropic's 1024-token
	// minimum across English / Chinese without a tokenizer
	// dependency. 4096 chars ≈ 1024–2048 tokens depending on script;
	// conservative — under-cache rather than burn the 25% write
	// surcharge on a too-short prefix that won't qualify server-side.
	anthropicCacheMinChars = 4096

	// anthropicMaxCacheBreakpoints is Anthropic's hard server-side
	// limit. Excess cache_control markers are rejected.
	anthropicMaxCacheBreakpoints = 4

	// anthropicMinMessagesForHistoryCache gates the history anchor:
	// single-turn calls won't benefit (no second call within the
	// 5-min ephemeral TTL to hit the cache), so the heuristic skips
	// short conversations. 4 covers (system, user, assistant, user)
	// which is the shortest meaningful "multi-turn" shape.
	anthropicMinMessagesForHistoryCache = 4
)

// anchorPlan describes the cache_control placement decided by
// planCacheAnchors. Zero values mean "no anchor of that kind".
type anchorPlan struct {
	// systemBlocks lists indices into the system []TextBlockParam
	// slice that should receive cache_control. Ordered by
	// placement priority (latest = highest); a caller can therefore
	// trim from the front when intersecting with a smaller budget.
	systemBlocks []int

	// historyMsgIdx is the index into the []MessageParam slice whose
	// final content block receives cache_control, caching all prior
	// turns up to and including that message. -1 = no history anchor.
	historyMsgIdx int

	// toolsLast is true when the final ToolUnionParam should receive
	// cache_control, caching the entire tool array as a unit.
	toolsLast bool
}

func newAnchorPlan() anchorPlan { return anchorPlan{historyMsgIdx: -1} }

// planCacheAnchors decides where to place Anthropic cache_control
// breakpoints across the request, respecting the global 4-breakpoint
// budget. Inputs are the already-converted Anthropic SDK params so
// the helper can measure segment sizes directly without re-walking
// the llm.Message slice.
//
// Priority order (descending value of cached prefix):
//
//  1. tools-end — tool definitions are large and the most stable
//     part of any agent's request; one anchor covers all tools.
//  2. history — caches every turn before the final user message;
//     biggest win on multi-turn conversations.
//  3. system-last — caches the entire system prefix including all
//     prior segments.
//  4. earlier system segments — each acts as a shorter fallback
//     anchor when later prefixes don't match (e.g. a volatile
//     segment in the middle invalidates the longer prefixes).
//
// Returns plan describing where to apply cache_control; caller must
// then mutate the SDK params accordingly.
func planCacheAnchors(system []asdk.TextBlockParam, msgs []asdk.MessageParam, tools []asdk.ToolUnionParam) anchorPlan {
	plan := newAnchorPlan()
	budget := anthropicMaxCacheBreakpoints

	if budget > 0 && hasLongTools(tools) {
		plan.toolsLast = true
		budget--
	}

	if budget > 0 && len(msgs) >= anthropicMinMessagesForHistoryCache {
		// Total content length of every turn *before* the final user
		// message — that final turn is what we want to be the "fresh"
		// part hitting against the cached prefix.
		var historyLen int
		for i := 0; i < len(msgs)-1; i++ {
			historyLen += msgContentLen(msgs[i])
		}
		if historyLen >= anthropicCacheMinChars {
			plan.historyMsgIdx = len(msgs) - 2
			budget--
		}
	}

	// System segments: walk last → first, anchor each long-enough
	// segment until budget exhausted. Anthropic's matching picks the
	// longest prefix that's identical to a prior call, so the latest
	// (most-specific) anchor is the most valuable; dropping earlier
	// anchors only loses shorter fallback caches.
	for i := len(system) - 1; i >= 0 && budget > 0; i-- {
		if len(system[i].Text) >= anthropicCacheMinChars {
			plan.systemBlocks = append(plan.systemBlocks, i)
			budget--
		}
	}

	return plan
}

// hasLongTools reports whether the serialized tool definitions are big
// enough to be worth caching. Each tool is roughly Description plus a
// JSON-encoded InputSchema; summing those across all tools gives a
// stable proxy that doesn't depend on a tokenizer.
func hasLongTools(tools []asdk.ToolUnionParam) bool {
	if len(tools) == 0 {
		return false
	}
	var total int
	for _, t := range tools {
		if t.OfTool == nil {
			continue
		}
		total += len(t.OfTool.Name)
		if t.OfTool.Description.Valid() {
			total += len(t.OfTool.Description.Value)
		}
		// InputSchema is param-encoded; marshal to measure. Errors
		// are squashed because (a) MarshalJSON on the SDK param is
		// effectively infallible for well-formed schemas, and
		// (b) on the off-chance it fails we fall back to "skip
		// anchor", which is the safe default.
		if b, err := json.Marshal(t.OfTool.InputSchema); err == nil {
			total += len(b)
		}
		if total >= anthropicCacheMinChars {
			return true
		}
	}
	return total >= anthropicCacheMinChars
}

// msgContentLen sums the text length across all text blocks of a
// MessageParam. Non-text blocks (tool_use, tool_result, image) are
// counted by their JSON-encoded size as a rough proxy — they
// participate in the cached prefix just like text does.
func msgContentLen(m asdk.MessageParam) int {
	var total int
	for _, blk := range m.Content {
		if t := blk.OfText; t != nil {
			total += len(t.Text)
			continue
		}
		if b, err := json.Marshal(blk); err == nil {
			total += len(b)
		}
	}
	return total
}

// applyAnchorsToSystem stamps cache_control on the system blocks
// selected by the plan. Mutates in place.
func applyAnchorsToSystem(system []asdk.TextBlockParam, indices []int) {
	for _, i := range indices {
		if i < 0 || i >= len(system) {
			continue
		}
		system[i].CacheControl = asdk.NewCacheControlEphemeralParam()
	}
}

// applyAnchorToHistory stamps cache_control on the final content block
// of msgs[msgIdx]. The final block — not the message as a whole — is
// where Anthropic expects the marker; everything up to and including
// that block becomes the cached prefix.
func applyAnchorToHistory(msgs []asdk.MessageParam, msgIdx int) {
	if msgIdx < 0 || msgIdx >= len(msgs) {
		return
	}
	content := msgs[msgIdx].Content
	if len(content) == 0 {
		return
	}
	// Walk the union to find the last block that owns a CacheControl
	// field; the SDK exposes a getter that returns nil for blocks
	// that don't support it (rare in practice — text/image/tool_use/
	// tool_result all do).
	last := &content[len(content)-1]
	if cc := last.GetCacheControl(); cc != nil {
		*cc = asdk.NewCacheControlEphemeralParam()
	}
}

// applyAnchorToTools stamps cache_control on the final tool, caching
// the entire tools array as a cacheable unit (the marker on the last
// tool covers all prior tools per Anthropic's prefix-cache semantics).
func applyAnchorToTools(tools []asdk.ToolUnionParam) {
	if len(tools) == 0 {
		return
	}
	last := &tools[len(tools)-1]
	if last.OfTool == nil {
		return
	}
	last.OfTool.CacheControl = asdk.NewCacheControlEphemeralParam()
}

// --- Beta variants (JSON-mode path) -----------------------------------
//
// The Beta Messages API uses a parallel type hierarchy with the same
// wire shape but different Go types (BetaTextBlockParam vs.
// TextBlockParam, BetaCacheControlEphemeralParam vs. its non-beta
// twin, …). The cache-control semantics are identical, so we reuse
// planCacheAnchors against the stable types and then re-apply the
// resulting plan to the converted beta params.
//
// JSON mode currently doesn't support tools at the adapter level —
// see applyBetaOptions — so the beta path skips the tools-end
// anchor. If tools are ever added to the beta surface, mirror
// applyAnchorToTools here against ToolUnionParam's beta cousin.

// applyAnchorsToBetaSystem mirrors applyAnchorsToSystem for
// BetaTextBlockParam, the SDK type used by the Beta Messages API.
func applyAnchorsToBetaSystem(system []asdk.BetaTextBlockParam, indices []int) {
	for _, i := range indices {
		if i < 0 || i >= len(system) {
			continue
		}
		system[i].CacheControl = asdk.NewBetaCacheControlEphemeralParam()
	}
}

// applyAnchorToBetaHistory mirrors applyAnchorToHistory for
// BetaMessageParam. The Beta union's GetCacheControl helper returns
// a *BetaCacheControlEphemeralParam, parallel to the stable variant.
func applyAnchorToBetaHistory(msgs []asdk.BetaMessageParam, msgIdx int) {
	if msgIdx < 0 || msgIdx >= len(msgs) {
		return
	}
	content := msgs[msgIdx].Content
	if len(content) == 0 {
		return
	}
	last := &content[len(content)-1]
	if cc := last.GetCacheControl(); cc != nil {
		*cc = asdk.NewBetaCacheControlEphemeralParam()
	}
}
