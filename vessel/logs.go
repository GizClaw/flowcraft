package vessel

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
)

// LogEntryType discriminates the variants a [LogEntry] can carry.
//
// The five values cover the full engine event contract surfaced to
// vessel observers: two run-level lifecycle markers, three step-level
// lifecycle markers, and the in-flight stream-delta channel.
// Graph-private subjects (parallel.fork / parallel.join) are NOT
// surfaced here — they are not part of the engine contract and would
// leak runner-specific structure into the daemon's public log API.
//
// "skipped" is included even though it is technically graph-private,
// because it occupies the same step lifecycle slot operators expect to
// see on a dashboard ("did this node run, fail, or skip?"). Subjects
// the runtime does not recognise pass through with [LogEntryUnknown]
// — consumers may either ignore unknowns (forward-compat) or surface
// them verbatim under their raw subject string.
type LogEntryType string

const (
	// LogEntryRunStarted maps to engine.SubjectRunStart — the once-
	// per-run "execution begun" marker. Payload carries the engine-
	// emitted start record (graph executor: the initial Board
	// vars). Consumers SHOULD treat this as the canonical "tail
	// has begun observing run X" anchor, not the
	// [Captain.Submit] return.
	LogEntryRunStarted LogEntryType = "run.started"

	// LogEntryRunEnded maps to engine.SubjectRunEnd. Payload carries
	// the engine's end record. Engines that fail mid-run still emit
	// this subject; the failure detail lives in the corresponding
	// step.error event(s) and (optionally) in the run-end payload.
	LogEntryRunEnded LogEntryType = "run.ended"

	// LogEntryStepStarted maps to engine.SubjectStepStart. ActorID
	// is set to the engine's actor identifier (graph: node id).
	LogEntryStepStarted LogEntryType = "step.started"

	// LogEntryStepEnded maps to engine.SubjectStepComplete (success
	// path) and engine.SubjectStepError (failure path) — both fold
	// to the same LogEntry type so consumers see a clean
	// step.started→step.ended pair regardless of outcome. The
	// outcome lives in Payload (status / error fields).
	LogEntryStepEnded LogEntryType = "step.ended"

	// LogEntryStepSkipped maps to graph runner's "skipped" subject.
	// Graph-private, but surfaced because operators reasonably
	// expect to see "this node was skipped" alongside started /
	// ended on a per-node lifecycle view.
	LogEntryStepSkipped LogEntryType = "step.skipped"

	// LogEntryStreamDelta maps to engine.SubjectStreamDelta. Payload
	// is a [engine.StreamDeltaPayload] (token / tool_call /
	// tool_result). The high-frequency channel.
	LogEntryStreamDelta LogEntryType = "stream.delta"

	// LogEntryUnknown is the catch-all for engine.run.<id>.>
	// envelopes the runtime does not recognise (typically engine-
	// private subjects added by future executors). Surfaced rather
	// than dropped so a graph-runner-specific dashboard can still
	// see them under the original subject; the canonical decoded
	// shape is "we don't know — here's the raw envelope".
	LogEntryUnknown LogEntryType = "unknown"
)

// LogEntry is one element delivered on the channel returned by
// [Logs] / [LogsForRun]. It is the unified, JSON-friendly projection
// of every engine.run.<id>.> envelope the captain's bus carries —
// designed so a daemon can fan it out as a single SSE stream where
// the SSE "event:" line carries [LogEntry.Type] and "data:" carries
// the JSON form of LogEntry itself.
//
// The struct is shaped for cross-process consumption: every field is
// either a primitive or a map[string]any (Payload), and JSON tags use
// snake_case to match the daemon's HTTP API conventions.
type LogEntry struct {
	// Type discriminates the payload shape. See [LogEntryType] for
	// the full enum and per-type contracts.
	Type LogEntryType `json:"type"`

	// RunID is the value extracted from event.HeaderRunID on the
	// originating envelope. Matches the agent.Run identifier and
	// the Handle.RunID returned by [Captain.Submit].
	RunID string `json:"run_id"`

	// ActorID is the engine actor — for graph runs this is the
	// node id. Empty on run-level events (run.started /
	// run.ended). Useful for distinguishing LLM tokens from
	// tool-call deltas in the same run.
	ActorID string `json:"actor_id,omitempty"`

	// Subject is the original event.Subject string. Carried so
	// dashboards can surface "engine-private" subjects (those
	// surfaced as Type=unknown) without losing routing context.
	Subject string `json:"subject"`

	// Seq is a per-RunID monotonically increasing counter assigned
	// at log-stream emission time (NOT carried on the underlying
	// envelope). Consumers MUST use Seq to detect dropped events
	// or out-of-order delivery within a single subscription.
	//
	// Seq is reset implicitly on a fresh subscription — i.e. it
	// counts from 1 within ONE call to [Logs] / [LogsForRun], not
	// since vessel boot. This avoids surfacing the captain's
	// per-run state to consumers and keeps the contract simple
	// ("first event you see has Seq=1").
	Seq int64 `json:"seq"`

	// Ts is the originating envelope's [event.Envelope.Time]. For
	// engines that fill it (the in-process bus does), this is the
	// publish time. Consumers SHOULD prefer Seq for ordering
	// because Ts has wall-clock-jitter caveats.
	Ts time.Time `json:"ts"`

	// Payload is the decoded payload of the originating envelope.
	// The JSON-shape is preserved verbatim; consumers discriminate
	// via Type:
	//
	//	Type=stream.delta             → fields of engine.StreamDeltaPayload
	//	Type=run.started              → executor-specific (graph: {Vars: ...})
	//	Type=run.ended                → executor-specific (graph: {status: "success" | "error", ...})
	//	Type=step.started             → executor-specific node-input projection
	//	Type=step.ended               → executor-specific (status/error/duration)
	//	Type=step.skipped             → executor-specific
	//	Type=unknown                  → whatever bytes the producer sent
	//
	// nil Payload is a valid envelope — engines may emit
	// no-payload markers (e.g. parallel.fork). Consumers should
	// guard against it.
	Payload map[string]any `json:"payload,omitempty"`
}

// Logs subscribes to every event of every run in the vessel and
// returns a channel of decoded [LogEntry]. The channel closes when
// ctx is cancelled or the underlying bus is closed (typically via
// [Captain.Stop]).
//
// Logs is the recommended entry point for SSE bridges, dashboards,
// CLI tail commands — anything that wants a live feed of run
// lifecycle, step lifecycle, and in-flight model output. For
// per-run scoping, use [LogsForRun] which adds a run-id filter on
// top of Logs.
//
// Returned errors are classified via sdk/errdefs (NotAvailable when
// the bus is nil, e.g. against a never-Launched Captain).
func Logs(ctx context.Context, c *Captain) (<-chan LogEntry, error) {
	if c == nil || c.bus == nil {
		return nil, errdefs.NotAvailablef("vessel: Captain has no bus")
	}
	sub, err := c.bus.Subscribe(ctx, event.Pattern("engine.run.>"))
	if err != nil {
		return nil, err
	}
	return logsFromSubscription(ctx, sub, ""), nil
}

// LogsForRun is the per-run variant of Logs. It returns only entries
// whose RunID matches the supplied id; entries from other runs are
// dropped at the consumer-facing channel boundary so callers see
// nothing about runs they did not ask about.
//
// The filter is post-subscribe (not subject-level) because engine
// subjects are sanitised — the runID may have lost characters and
// exact subject matching becomes brittle. Routing on the
// HeaderRunID is the canonical correlation key.
func LogsForRun(ctx context.Context, c *Captain, runID string) (<-chan LogEntry, error) {
	if c == nil || c.bus == nil {
		return nil, errdefs.NotAvailablef("vessel: Captain has no bus")
	}
	if runID == "" {
		return nil, errdefs.Validationf("vessel: LogsForRun requires non-empty runID")
	}
	sub, err := c.bus.Subscribe(ctx, event.Pattern("engine.run.>"))
	if err != nil {
		return nil, err
	}
	return logsFromSubscription(ctx, sub, runID), nil
}

// logsFromSubscription is the shared decode + filter loop. It exits
// when sub closes or ctx fires, closing the consumer-facing channel
// so callers' range loops terminate cleanly.
//
// Two design notes worth flagging:
//
//   - Per-RunID Seq is assigned AT EMISSION inside this loop, not on
//     the envelope. The bus has no global ordering guarantee
//     (different subscribers may see different orders if the bus
//     fans out via goroutines), so emitting Seq here makes
//     "monotonic per (subscription, run)" a tight, locally-checkable
//     invariant.
//
//   - Unknown subjects fall through as Type=unknown rather than being
//     dropped silently. This keeps the API forward-compatible (a new
//     graph-private subject lands as data, not as a missing event)
//     and lets dashboards surface them under their raw subject for
//     diagnostic purposes.
func logsFromSubscription(ctx context.Context, sub event.Subscription, runFilter string) <-chan LogEntry {
	out := make(chan LogEntry, 32)
	go func() {
		defer close(out)
		defer sub.Close()
		// seqByRun is local to the goroutine; no lock needed
		// because exactly this goroutine writes to it. Map type
		// (not slice) because RunIDs are sparse strings, not
		// dense indices.
		seqByRun := map[string]int64{}
		var mu sync.Mutex // future-proof: tests may want to peek
		for {
			select {
			case env, ok := <-sub.C():
				if !ok {
					return
				}
				runID := env.Headers[event.HeaderRunID]
				if runFilter != "" && runID != runFilter {
					continue
				}
				entryType, actor := classifySubject(env.Subject)
				if actor == "" {
					actor = env.Headers[event.HeaderActorID]
				}
				mu.Lock()
				seqByRun[runID]++
				seq := seqByRun[runID]
				mu.Unlock()

				entry := LogEntry{
					Type:    entryType,
					RunID:   runID,
					ActorID: actor,
					Subject: string(env.Subject),
					Seq:     seq,
					Ts:      env.Time,
					Payload: decodeEnvelopePayload(env),
				}
				select {
				case out <- entry:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// classifySubject walks the suffix structure of an engine.run.* subject
// and folds it into a [LogEntryType] + actorID pair. Returns
// LogEntryUnknown for subjects we don't recognise, leaving the routing
// surface forward-compatible.
//
// Order of checks matters: stream.delta is the high-frequency case so
// it goes first; step.* before the generic run-level start/end so we
// don't mis-classify "...step.<id>.start" as run.started.
func classifySubject(s event.Subject) (LogEntryType, string) {
	if engine.IsStreamDelta(s) {
		return LogEntryStreamDelta, extractActor(string(s), ".stream.", ".delta")
	}
	str := string(s)
	if !strings.HasPrefix(str, engine.SubjectPrefix) {
		return LogEntryUnknown, ""
	}
	switch {
	case strings.Contains(str, ".step.") && strings.HasSuffix(str, ".start"):
		return LogEntryStepStarted, extractActor(str, ".step.", ".start")
	case strings.Contains(str, ".step.") && strings.HasSuffix(str, ".complete"):
		return LogEntryStepEnded, extractActor(str, ".step.", ".complete")
	case strings.Contains(str, ".step.") && strings.HasSuffix(str, ".error"):
		return LogEntryStepEnded, extractActor(str, ".step.", ".error")
	case strings.Contains(str, ".step.") && strings.HasSuffix(str, ".skipped"):
		return LogEntryStepSkipped, extractActor(str, ".step.", ".skipped")
	case strings.HasSuffix(str, ".start"):
		return LogEntryRunStarted, ""
	case strings.HasSuffix(str, ".end"):
		return LogEntryRunEnded, ""
	}
	return LogEntryUnknown, ""
}

// extractActor pulls the actor segment out of a subject shaped
// "...<sep>actor<suffix>". Returns "" on shape mismatch — a
// classification fallback rather than a hard error because the
// classifier already filtered to subjects we expect to match.
func extractActor(subject, sep, suffix string) string {
	idx := strings.Index(subject, sep)
	if idx < 0 {
		return ""
	}
	rest := subject[idx+len(sep):]
	if !strings.HasSuffix(rest, suffix) {
		return ""
	}
	return rest[:len(rest)-len(suffix)]
}

// decodeEnvelopePayload tries hard to give consumers a map-shaped
// payload regardless of how the producer encoded it. JSON object →
// map; JSON scalar / array / nil → wrapped under {"value": ...} so
// the LogEntry.Payload type stays uniform. This is what makes the
// SSE wire format "data: {type:...,payload:{...}}" predictable for
// generic clients.
func decodeEnvelopePayload(env event.Envelope) map[string]any {
	if len(env.Payload) == 0 {
		return nil
	}
	var asMap map[string]any
	if err := json.Unmarshal(env.Payload, &asMap); err == nil && asMap != nil {
		return asMap
	}
	var generic any
	if err := json.Unmarshal(env.Payload, &generic); err == nil {
		return map[string]any{"value": generic}
	}
	// Bytes that don't decode as JSON: surface them as an opaque
	// string so the consumer can still log them.
	return map[string]any{"raw": string(env.Payload)}
}
