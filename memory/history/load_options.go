package history

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// LoadOptions filters the messages returned by [LoadFiltered] / a
// [FilterableHistory] implementation. Zero values mean "no filter on this
// dimension" — the empty LoadOptions is identical to a plain Load with
// Budget==zero.
//
// LoadOptions is the moderation-friendly counterpart to [Budget]:
// where Budget caps how much transcript reaches the LLM, LoadOptions
// shapes which slice of the transcript is read for inspection,
// debugging, audit views, or selective replay.
//
// Filter semantics are evaluated AFTER the underlying History strategy
// returns its working set:
//
//   - Budget: forwarded to the underlying Load. Implementations apply
//     compaction / windowing the same way they always do.
//   - Roles:  if non-empty, only messages whose Role is in the set are
//     kept. The empty set means "all roles".
//   - SinceSeq: 0-based message sequence index. Messages at index <
//     SinceSeq are dropped. SinceSeq is a position cutoff because
//     [model.Message] does not carry a wall-clock timestamp; callers
//     wanting a time cutoff should look up the sequence index in their
//     own audit log first. SinceSeq is applied against the position in
//     the slice returned by Load (i.e. AFTER compaction), so it is
//     stable for callers that always pass the same Budget. Negative
//     values are treated as 0.
//   - LimitN: caps the number of messages returned AFTER role +
//     SinceSeq filtering. 0 means "no cap"; the most recent LimitN
//     surviving messages are kept (tail-biased to match the typical
//     "show me the last N user/assistant turns" use case).
//   - IncludeTools: when false (the default), strips tool-call /
//     tool-result parts as well as RoleTool messages from the result.
//     This matches the common moderation case where reviewers want the
//     human-readable conversation only. When true, tool messages and
//     tool-call parts are preserved verbatim.
//
// Filters compose: callers can mix Roles + LimitN to e.g. "give me the
// last 20 assistant messages". The empty LoadOptions is a no-op and
// returns whatever the underlying Load produces.
type LoadOptions struct {
	Budget       Budget
	Roles        []model.Role
	SinceSeq     int
	LimitN       int
	IncludeTools bool
}

// IsZero reports whether opts carries no explicit filters and is
// equivalent to a plain Load with the zero Budget.
func (opts LoadOptions) IsZero() bool {
	return opts.Budget.IsZero() &&
		len(opts.Roles) == 0 &&
		opts.SinceSeq <= 0 &&
		opts.LimitN <= 0 &&
		!opts.hasIncludeTools()
}

// hasIncludeTools is a small helper so a future change that inverts the
// default for IncludeTools (or makes it a tri-state) only needs to
// touch one spot.
func (opts LoadOptions) hasIncludeTools() bool {
	// IncludeTools=false is the documented default and means "strip"; we
	// don't treat that as a non-zero filter, otherwise an empty struct
	// would not satisfy IsZero().
	return false
}

// FilterableHistory is the optional sub-interface that History
// implementations can satisfy to honour LoadOptions efficiently. For
// example, a store-backed [History] may push role filtering down to
// the database instead of materialising the whole conversation.
//
// History implementations that do NOT satisfy FilterableHistory still
// work with [LoadFiltered]: the helper falls back to a plain Load and
// applies the filters in memory. This keeps adding the new entry point
// non-breaking for downstream code that built its own History.
type FilterableHistory interface {
	History
	// LoadFiltered returns messages for the next inspection / display
	// the same way [History.Load] does, additionally honouring
	// LoadOptions. Implementations MAY apply filters lazily (push down
	// to the store) or eagerly (call Load + filter); both are correct
	// as long as the result respects LoadOptions semantics documented
	// above.
	LoadFiltered(ctx context.Context, conversationID string, opts LoadOptions) ([]model.Message, error)
}

// LoadFiltered returns the messages selected by opts. If h satisfies
// [FilterableHistory] the call is delegated to it; otherwise this
// helper performs a plain h.Load(ctx, conversationID, opts.Budget) and
// applies the filters in memory.
//
// LoadFiltered is the recommended entry point for callers that need
// post-Load filtering: it picks the most efficient path automatically
// and shields callers from the FilterableHistory type assertion.
func LoadFiltered(ctx context.Context, h History, conversationID string, opts LoadOptions) ([]model.Message, error) {
	if fh, ok := h.(FilterableHistory); ok {
		return fh.LoadFiltered(ctx, conversationID, opts)
	}
	msgs, err := h.Load(ctx, conversationID, opts.Budget)
	if err != nil {
		return nil, err
	}
	return ApplyLoadOptions(msgs, opts), nil
}

// ApplyLoadOptions filters msgs in memory according to opts. It is the
// reference implementation reused by both the in-memory fallback in
// [LoadFiltered] and any FilterableHistory implementation that prefers
// to do its own Load and then defer filtering to the shared helper.
//
// The returned slice is a fresh allocation; the input slice is not
// mutated. msgs[i] are NOT cloned — IncludeTools=false strips
// tool-bearing Parts via [model.Message.Clone] only on the messages
// that actually carry them, so callers can rely on the returned slice
// being safe to retain.
func ApplyLoadOptions(msgs []model.Message, opts LoadOptions) []model.Message {
	if opts.IsZero() {
		return msgs
	}

	allowedRoles := roleSet(opts.Roles)

	// We filter in three passes for clarity rather than fusing them; a
	// transcript bigger than a few thousand messages is exceptional and
	// the underlying Load is the dominant cost.
	out := make([]model.Message, 0, len(msgs))
	for i, m := range msgs {
		if opts.SinceSeq > 0 && i < opts.SinceSeq {
			continue
		}
		if allowedRoles != nil {
			if _, ok := allowedRoles[m.Role]; !ok {
				continue
			}
		}
		if !opts.IncludeTools {
			if m.Role == model.RoleTool {
				continue
			}
			if hasToolBearingPart(m) {
				m = stripToolParts(m)
				if len(m.Parts) == 0 {
					// A pure tool-call assistant message becomes empty
					// once tool parts are stripped; skip it so the
					// caller does not see a stub turn.
					continue
				}
			}
		}
		out = append(out, m)
	}

	if opts.LimitN > 0 && len(out) > opts.LimitN {
		out = out[len(out)-opts.LimitN:]
	}
	return out
}

// roleSet builds a small lookup set from the slice form callers
// typically pass on the wire. Returns nil for the empty input so the
// "no filter" path stays branch-free.
func roleSet(roles []model.Role) map[model.Role]struct{} {
	if len(roles) == 0 {
		return nil
	}
	m := make(map[model.Role]struct{}, len(roles))
	for _, r := range roles {
		m[r] = struct{}{}
	}
	return m
}

// hasToolBearingPart reports whether m carries any tool-call or
// tool-result parts. Used to short-circuit the strip pass for the
// common case of a plain text turn.
func hasToolBearingPart(m model.Message) bool {
	for _, p := range m.Parts {
		if p.Type == model.PartToolCall || p.Type == model.PartToolResult {
			return true
		}
	}
	return false
}

// stripToolParts returns a clone of m with PartToolCall / PartToolResult
// parts removed. Other Parts (text, image, audio, file, data) are
// retained verbatim. The returned message is a deep copy so the caller
// can mutate either slice without aliasing.
func stripToolParts(m model.Message) model.Message {
	cp := m.Clone()
	kept := cp.Parts[:0]
	for _, p := range cp.Parts {
		if p.Type == model.PartToolCall || p.Type == model.PartToolResult {
			continue
		}
		kept = append(kept, p)
	}
	cp.Parts = kept
	return cp
}
