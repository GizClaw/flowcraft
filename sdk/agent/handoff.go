package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// Handoff describes a controlled hand-off from the *current* agent
// to a *target* agent. Hand-offs are exposed to the LLM as ordinary
// tools — calling the tool is the LLM's way of saying "I want to
// transfer this conversation to <target>". The agent layer detects
// that call after [Run] returns and writes a structured
// [HandoffEvent] into [Result.State] under [HandoffStateKey] so the
// host can dispatch the next turn (typically via sdk/kanban).
//
// The DSL is deliberately data-shaped: a Handoff value can be
// declared next to the Agent, marshalled to JSON for an admin UI,
// and inspected without booting the runtime. Attaching hand-offs
// to a turn is two lines:
//
//	hs := []agent.Handoff{
//	    {ToAgentID: "billing", Description: "Refunds, invoices, plans"},
//	    {ToAgentID: "tech",    Description: "Bugs, errors, integrations"},
//	}
//	tools := append(baseTools, agent.HandoffTools(hs)...)
//	deciders := []agent.Decider{agent.HandoffDecider(hs)}
//
// Recommended host loop after Run returns:
//
//	if ev, ok := agent.HandoffFromResult(res); ok {
//	    next := dispatch(ev.ToAgentID, ev.Note)  // kanban / direct Run / queue
//	    return next, nil
//	}
//
// Hand-offs do not fork: only one hand-off per turn is honoured.
// When the LLM calls multiple hand-off tools in the same turn the
// FIRST call wins; subsequent hand-off tool calls are dropped at
// detection time. This matches user expectation ("transfer me to
// billing — actually no, tech" -> follow the user's last word) and
// avoids ambiguous double-dispatch.
type Handoff struct {
	// ToAgentID is the stable identifier of the receiving agent.
	// The host is responsible for resolving this id to a runnable
	// Agent + Engine pair; the SDK stays out of routing.
	ToAgentID string

	// Description is shown to the LLM as the tool description so
	// it knows when to invoke the hand-off. Keep it short and
	// behavioural ("Refunds, invoices, plans"). Empty falls back
	// to a generic "Transfer the conversation to <id>".
	Description string

	// ToolName overrides the LLM-facing tool name. Default is
	// "transfer_to_<sanitised_ToAgentID>" (lowercased,
	// non-alphanumeric → "_"). Override when two hand-offs would
	// otherwise collide on the default name (e.g. multiple
	// agents whose IDs differ only in case).
	ToolName string

	// Filter, when set, is consulted by [HandoffTools] to allow
	// per-request gating without rebuilding the slice. A Filter
	// returning false hides the hand-off from this turn's LLM.
	// Typical use: tenant-aware gating, permission checks.
	Filter func(ctx context.Context, req *Request) bool

	// OnInvoke fires synchronously inside the hand-off tool's
	// Execute call. Use it for lightweight observability (a log
	// line, a metric) — heavy work should be done by the host
	// after Run returns. Returning an error from OnInvoke fails
	// the tool call and the LLM may retry; the hand-off is NOT
	// recorded in that case.
	OnInvoke func(ctx context.Context, args HandoffArgs) error
}

// HandoffArgs is the LLM-supplied JSON-decoded argument bundle for
// a hand-off tool call. Only Reason / Note are exposed — the SDK
// purposefully refuses richer parameter shapes to keep the schema
// uniform across hand-offs (different shapes per hand-off encourage
// the LLM to leak structured data into a free-form field). Hosts
// that need richer dispatch data should attach it via Request
// metadata before scheduling the next turn, not via the hand-off
// tool itself.
type HandoffArgs struct {
	// Reason is a short rationale for the transfer. Surfaced to
	// the receiving agent and to telemetry. Example:
	// "User asked about invoice for order #1234".
	Reason string `json:"reason,omitempty"`

	// Note is an optional free-form message the LLM wants the
	// receiving agent to read first. Avoid stuffing the entire
	// transcript here; the receiving side should reload transcript
	// context via the host's normal mechanisms.
	Note string `json:"note,omitempty"`
}

// HandoffEvent is the structured record placed in [Result.State]
// under [HandoffStateKey] when [HandoffDecider] detects a hand-off
// tool call. The host consumes this to dispatch the next turn.
type HandoffEvent struct {
	// ToAgentID mirrors [Handoff.ToAgentID].
	ToAgentID string `json:"to_agent_id"`

	// ToolName is the tool name the LLM actually invoked.
	ToolName string `json:"tool_name"`

	// ToolCallID echoes the ToolCall.ID from the model, so the
	// host can correlate downstream events back to the originating
	// call.
	ToolCallID string `json:"tool_call_id"`

	// Args carries the LLM-supplied reason / note.
	Args HandoffArgs `json:"args,omitempty"`
}

// HandoffStateKey is the [Result.State] map key under which
// [HandoffDecider] writes its [HandoffEvent]. Exposed as a constant
// so hosts and Observers can probe the state without depending on
// the decider's import path.
const HandoffStateKey = "handoff"

// HandoffFinalizeReason is the conventional [FinalizeDecision.Reason]
// prefix used by [HandoffDecider]. Format: "handoff:<to_agent_id>".
// Telemetry consumers can branch on the prefix without parsing the
// full state map.
const HandoffFinalizeReason = "handoff:"

// HandoffTool returns one [tool.Tool] that exposes h to the LLM.
// The tool's Execute body is intentionally a no-op (returns a short
// confirmation string) — actual dispatch is the host's job, after
// observing [HandoffEvent] in [Result.State]. This separation keeps
// the LLM round happy (it sees a successful tool result and finishes
// its turn cleanly) without giving the tool surface any control over
// the receiving agent's wiring.
//
// HandoffTool panics if h.ToAgentID is empty: a hand-off without a
// destination is a programming error best caught at registration
// time.
func HandoffTool(h Handoff) tool.Tool {
	if h.ToAgentID == "" {
		panic("agent.HandoffTool: Handoff.ToAgentID is required")
	}
	name := h.ToolName
	if name == "" {
		name = DefaultHandoffToolName(h.ToAgentID)
	}
	desc := h.Description
	if desc == "" {
		desc = "Transfer the conversation to " + h.ToAgentID
	}
	def := model.ToolDefinition{
		Name:        name,
		Description: desc,
		InputSchema: handoffInputSchema(),
	}
	return tool.FuncTool(def, func(ctx context.Context, args string) (string, error) {
		var parsed HandoffArgs
		if args != "" {
			// Tolerate empty / nil args bodies: not all model
			// providers populate the arguments string for
			// argument-less calls. We still validate JSON
			// shape when present so the host's HandoffEvent.Args
			// stays clean.
			if err := json.Unmarshal([]byte(args), &parsed); err != nil {
				return "", fmt.Errorf("agent: handoff %q: invalid args: %w", name, err)
			}
		}
		if h.OnInvoke != nil {
			if err := h.OnInvoke(ctx, parsed); err != nil {
				return "", err
			}
		}
		// The string returned to the LLM is short and uniform so
		// the model does not anchor on it stylistically. Hosts
		// that customise the receiving-side handoff message
		// SHOULD do so on the receiving turn, not here.
		return "Handoff initiated to " + h.ToAgentID, nil
	})
}

// HandoffTools converts a slice of [Handoff] into the matching
// slice of [tool.Tool]s, applying any per-Handoff Filter against
// req. Hand-offs whose Filter rejects req are silently omitted —
// the tool simply does not appear to the LLM that turn.
//
// Pass req=nil to skip filtering (e.g. for static analysis or
// admin UI listings).
func HandoffTools(ctx context.Context, req *Request, hs []Handoff) []tool.Tool {
	out := make([]tool.Tool, 0, len(hs))
	for _, h := range hs {
		if h.Filter != nil && req != nil && !h.Filter(ctx, req) {
			continue
		}
		out = append(out, HandoffTool(h))
	}
	return out
}

// HandoffDecider returns a [Decider] that scans Result.Messages for
// the FIRST tool call whose name matches one of hs and, when found,
// records the hand-off in Result.State and emits a
// [FinalizeDecision] with Reason = [HandoffFinalizeReason] +
// ToAgentID.
//
// Behaviour:
//
//   - Detection is read-only: the decider does not modify
//     Result.Messages. The hand-off tool's Execute already produced
//     a tool_result the LLM will see; suppressing the rest of the
//     turn is the host's job (typically by *not* committing the
//     result and dispatching the next agent).
//
//   - DiscardOutput is left at false. Hosts that want to drop the
//     LLM's pre-handoff prose should layer their own decider
//     downstream — the choice depends on whether the assistant's
//     "Sure, transferring you now…" line should appear in the
//     transcript.
//
//   - Multiple hand-offs in one turn are deduped to the first
//     match (see Handoff doc).
//
// The returned Decider stores the [HandoffEvent] under
// [HandoffStateKey]; consumers retrieve it via
// [HandoffFromResult] which performs the type assertion + map
// initialisation safely.
func HandoffDecider(hs []Handoff) Decider {
	if len(hs) == 0 {
		return BaseDecider{}
	}
	// Build a name → handoff lookup once so per-turn detection
	// stays O(message * tool_call) instead of O(× len(hs)).
	byName := make(map[string]Handoff, len(hs))
	for _, h := range hs {
		name := h.ToolName
		if name == "" {
			name = DefaultHandoffToolName(h.ToAgentID)
		}
		byName[name] = h
	}
	return &handoffDecider{lookup: byName}
}

type handoffDecider struct {
	BaseDecider
	lookup map[string]Handoff
}

// BeforeFinalize implements [Decider]. It walks res.Messages from
// the FIRST message forward (the LLM's chronological order) so
// "first tool call wins" reflects the natural sequence the model
// produced.
func (d *handoffDecider) BeforeFinalize(_ context.Context, _ RunInfo, _ *Request, res *Result) (FinalizeDecision, error) {
	if res == nil {
		return FinalizeDecision{}, nil
	}
	for _, msg := range res.Messages {
		for _, tc := range msg.ToolCalls() {
			h, ok := d.lookup[tc.Name]
			if !ok {
				continue
			}
			ev := HandoffEvent{
				ToAgentID:  h.ToAgentID,
				ToolName:   tc.Name,
				ToolCallID: tc.ID,
			}
			if tc.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Arguments), &ev.Args)
			}
			if res.State == nil {
				res.State = map[string]any{}
			}
			res.State[HandoffStateKey] = ev
			return FinalizeDecision{
				Reason: HandoffFinalizeReason + h.ToAgentID,
			}, nil
		}
	}
	return FinalizeDecision{}, nil
}

// HandoffFromResult extracts the [HandoffEvent] previously written
// by [HandoffDecider]. Returns (zero, false) when no hand-off
// happened or when the state slot was overwritten with an
// unexpected type.
//
// Hosts use this in their dispatch loop:
//
//	res, _ := agent.Run(ctx, current, eng, req, opts...)
//	if ev, ok := agent.HandoffFromResult(res); ok {
//	    return dispatchTo(ev.ToAgentID, ev.Args.Note)
//	}
func HandoffFromResult(res *Result) (HandoffEvent, bool) {
	if res == nil || res.State == nil {
		return HandoffEvent{}, false
	}
	v, ok := res.State[HandoffStateKey]
	if !ok {
		return HandoffEvent{}, false
	}
	ev, ok := v.(HandoffEvent)
	return ev, ok
}

// DefaultHandoffToolName produces the canonical LLM-facing tool
// name from an agent id: "transfer_to_<sanitised>" where the
// sanitiser lowercases and replaces any non-[a-z0-9_] rune with
// "_". Mirrors what [HandoffTool] uses when [Handoff.ToolName] is
// empty so callers writing prompt templates can compute the same
// name without booting the SDK.
func DefaultHandoffToolName(agentID string) string {
	if agentID == "" {
		return "transfer_to_unknown"
	}
	var b strings.Builder
	b.Grow(len("transfer_to_") + len(agentID))
	b.WriteString("transfer_to_")
	for _, r := range agentID {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// handoffInputSchema returns the JSON-Schema for the structured
// arguments object surfaced to the LLM. Static and uniform across
// every hand-off tool — see the [HandoffArgs] doc for why we don't
// allow per-handoff schema customisation.
func handoffInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"reason": map[string]any{
				"type":        "string",
				"description": "Short rationale for the transfer (1 sentence).",
			},
			"note": map[string]any{
				"type":        "string",
				"description": "Optional message for the receiving agent to read first.",
			},
		},
		"additionalProperties": false,
	}
}

// Compile-time assertion that the decider does not accidentally
// drop the BaseDecider embedding (which provides the no-op default
// for any future Decider methods we might add).
var (
	_ Decider = (*handoffDecider)(nil)
	_         = errors.New
)
