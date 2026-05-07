package vessel

import (
	"context"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// historySeeder is the [agent.BoardSeeder] the Captain installs for
// any agent whose [spec.Agent.HistoryAccess] is ReadOnly or
// ReadWrite. It loads the prior transcript via [history.LoadFiltered]
// and prepends it to the seeded board so the engine sees the full
// conversation context — the new user turn is appended afterwards
// so it remains the last MainChannel message.
//
// The conversation key is taken from req.ContextID. When ContextID
// is empty the seeder falls through to the default behaviour (just
// the request message + inputs) — agents that want shared history
// MUST scope their requests under a stable ContextID.
type historySeeder struct {
	store  history.History
	access spec.HistoryAccess
}

// SeedBoard implements [agent.BoardSeeder].
func (s historySeeder) SeedBoard(ctx context.Context, _ agent.RunInfo, req *agent.Request) (*engine.Board, error) {
	b := engine.NewBoard()
	for k, v := range req.Inputs {
		b.SetVar(k, v)
	}
	if req.ContextID == "" || s.store == nil {
		// No shared transcript scope; behave like the agent default
		// seeder so callers can leave ContextID empty for one-off
		// requests without losing the per-turn message.
		b.AppendChannelMessage(engine.MainChannel, req.Message)
		return b, nil
	}

	// ReadWrite agents see the raw transcript including tool turns.
	// ReadOnly agents (typically moderators) get a clean human-
	// readable view by default — IncludeTools=false strips tool
	// messages / parts via history.LoadFiltered.
	opts := history.LoadOptions{
		IncludeTools: s.access == spec.HistoryAccessReadWrite,
	}
	prior, err := history.LoadFiltered(ctx, s.store, req.ContextID, opts)
	if err != nil {
		return nil, errdefs.Internalf("vessel: history load for %q: %v", req.ContextID, err)
	}
	for _, m := range prior {
		b.AppendChannelMessage(engine.MainChannel, m)
	}
	b.AppendChannelMessage(engine.MainChannel, req.Message)
	return b, nil
}

// historyAppender is the [agent.Observer] the Captain installs for
// any agent whose HistoryAccess is ReadWrite. It writes the user
// turn at OnRunStart and the assistant turn(s) at OnRunEnd into the
// shared transcript.
//
// Splitting the writes across the two callbacks (rather than batching
// on OnRunEnd) means a crash / Stop mid-Run still leaves the user
// turn persisted — without that, the next Load would surface an
// assistant reply with no question to answer against. The contract
// matches the "transcript appender" pattern called out in
// [agent.Observer]'s docs: short-circuit when Result.Committed is
// false (already enforced by the agent.Run default disposition for
// non-completed runs).
type historyAppender struct {
	agent.BaseObserver
	store history.History
}

// OnRunStart persists the user message before the engine sees it.
//
// Append errors are logged but NOT propagated: agent.Observer's
// contract says start/end callbacks cannot abort the run. A failed
// Append leaves the next turn's Load short of a user message, so
// surfacing the error here is the only way to make the inconsistency
// debuggable.
func (h historyAppender) OnRunStart(ctx context.Context, info agent.RunInfo, req *agent.Request) {
	if h.store == nil || info.ContextID == "" || req == nil {
		return
	}
	if isEmptyMessage(req.Message) {
		return
	}
	if err := h.store.Append(ctx, info.ContextID, []model.Message{req.Message}); err != nil {
		telemetry.Warn(ctx, "vessel: history append (user turn) failed",
			otellog.String("context_id", info.ContextID),
			otellog.String("error", err.Error()))
	}
}

// OnRunEnd persists the assistant turn(s) when the run committed.
// Non-committed runs (interrupt / fail / cancel) leave the user
// message visible but no assistant follow-up — the next Submit can
// retry against the same ContextID and the user turn is already there.
//
// Append errors are logged (see OnRunStart for the rationale).
func (h historyAppender) OnRunEnd(ctx context.Context, info agent.RunInfo, res *agent.Result) {
	if res == nil || !res.Committed || h.store == nil || info.ContextID == "" {
		return
	}
	if len(res.Messages) == 0 {
		return
	}
	if err := h.store.Append(ctx, info.ContextID, res.Messages); err != nil {
		telemetry.Warn(ctx, "vessel: history append (assistant turn) failed",
			otellog.String("context_id", info.ContextID),
			otellog.Int("messages", len(res.Messages)),
			otellog.String("error", err.Error()))
	}
}

// isEmptyMessage reports whether m has no Role / Content / Parts.
// Used to skip degenerate request appends — agents are sometimes
// invoked with a synthetic empty message (sidecar bus triggers
// that don't carry a chat turn) and we don't want those polluting
// the transcript.
func isEmptyMessage(m model.Message) bool {
	return m.Role == "" && m.Content() == "" && len(m.Parts) == 0
}

// buildHistoryStore constructs a [history.History] from the spec
// and the caller's options. Precedence is:
//
//  1. WithHistory(...) override — highest priority, lets callers
//     share a single store across vessels.
//  2. spec.History when set — buffer or compacted.
//  3. nil — no shared history (agents fall back to the default
//     seeder, OnRunStart/OnRunEnd observers become no-ops).
//
// Compacted histories require an llm.LLM for the summariser; v0.1.0
// keeps the wiring simple by deferring compacted construction to a
// future release that ships a workspace abstraction. Callers that
// need the compacted strategy today should construct it themselves
// and pass it via WithHistory.
func buildHistoryStore(vs spec.Spec, override history.History) (history.History, error) {
	if override != nil {
		return override, nil
	}
	if vs.History == nil {
		return nil, nil
	}
	switch vs.History.Kind {
	case "", "buffer":
		store := history.NewInMemoryStore()
		opts := []history.BufferOption{}
		if n := vs.History.MaxMessages; n > 0 {
			opts = append(opts, history.WithBufferMax(n))
		}
		return history.NewBuffer(store, opts...), nil
	case "compacted":
		return nil, errdefs.Validationf("vessel: spec.History.Kind=%q requires WithHistory(...) — compacted assembly is caller-side in v0.1.0", vs.History.Kind)
	default:
		return nil, errdefs.Validationf("vessel: spec.History.Kind=%q is invalid", vs.History.Kind)
	}
}
