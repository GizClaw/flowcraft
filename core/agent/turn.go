package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"runtime/debug"
	"time"

	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/identity/inject"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// spanTurn is the span one turn opens. It is the tick of the run's clock: without it a
// run has a start and an end and nothing in between.
const spanTurn = "agent.turn"

// spanCheckpointSave is the span one terminal save opens. It is the turn's disk clock: without
// it a slow store and a slow model are the same latency on the turn span.
const spanCheckpointSave = "checkpoint.save"

// eventInterrupted is the span event one applied operator input adds. It is an event rather
// than an attribute because a turn can absorb several, and their order is the story.
const eventInterrupted = "agent.interrupted"

// errNilDriver reports a host that called RunTurn without a driver, which is a wiring bug
// rather than a turn that failed.
var errNilDriver = errors.New("agent: RunTurn requires a driver")

// Event log kinds. The log is not the stream: stream deltas (text, reasoning, token
// counts) are the product surface and are not persisted per event, while the facts a
// resume or an audit needs are.
const (
	// recordState is a lifecycle transition.
	recordState = "state"
	// recordInterrupted is an operator input that was applied.
	recordInterrupted = "interrupted"
	// recordArtifact is a produced artifact reference.
	recordArtifact = "artifact"
)

// Turn runs one turn: it attaches the turn's identity, opens its span, drives the driver,
// forwards every event as it arrives, and saves the terminal checkpoint with the facts
// worth keeping.
//
// It is the one place turn-level cross-cutting lives. A driver that implemented this
// itself would re-derive the identity rules and the checkpoint sequence, and a driver
// written later would not implement it at all.
//
// Events are forwarded, never buffered: a caller may be streaming text, and turning that
// into a two-phase collect-then-emit would make a turn feel as slow as it is.
func RunTurn(ctx context.Context, req Request, d Driver) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if d == nil {
			yield(nil, errNilDriver)
			return
		}
		ctx = WithFailureBox(ctx)
		// The turn owns the driver's lifetime: when the consumer stops early, nothing
		// else will tell the driver to stop.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		ctx, span := startTurnSpan(ctx, req)
		defer span.End()

		// The saver is derived here, so a host passes a Store and a driver sees the
		// narrow half of it.
		if req.Saver == nil && req.Store != nil {
			req.Saver = storeSaver{store: req.Store}
		}

		started := time.Now()
		turn := TurnInfo{
			Agent:     req.Agent,
			SessionID: sessionID(req.Binding),
			TaskID:    req.Task.ID,
			Message:   req.Input,
		}
		if req.Hooks.BeforeTurn != nil {
			if err := req.Hooks.BeforeTurn(ctx, &turn); err != nil {
				RecordFailure(ctx, FailurePolicyDenied)
				finish(ctx, span, req, turn, TurnOutcome{Err: err}, nil, time.Since(started))
				yield(nil, err)
				return
			}
			req.Input = turn.Message
		}

		outcome, records, failure := driveTurn(ctx, cancel, req, d, yield)
		finish(ctx, span, req, turn, outcome, records, time.Since(started))
		if failure != nil {
			yield(nil, failure)
		}
	}
}

// driveTurn runs the driver and folds its stream into the turn's outcome, the facts worth
// persisting, and — when the driver panicked — the error the caller sees instead.
//
// The fold is why this loop reads every event: the terminal state, the usage totals and
// the interruptions are what the checkpoint and the record need, and only a reader that
// saw the whole stream can report them.
func driveTurn(ctx context.Context, cancel context.CancelFunc, req Request, d Driver, yield func(Event, error) bool) (outcome TurnOutcome, records []EventRecord, failure error) {
	completed := false
	// A panicking driver must not escape as a panic: the caller is often a fan-out
	// goroutine with nowhere to recover, and the turn would leave no terminal state.
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		cancel()
		RecordFailure(ctx, FailurePanicked)
		outcome.Err = fmt.Errorf("driver panicked: %v", recovered)
		outcome.State = StateFailed
		failure = outcome.Err
		telemetry.Warn(ctx, "agent: the driver panicked",
			attribute.String(telemetry.AttrErrorMessage, fmt.Sprint(recovered)),
			attribute.String(telemetry.AttrCodeStacktrace, string(debug.Stack())))
	}()

	for event, err := range d.Run(ctx, req) {
		if err != nil {
			outcome.Err = err
			if _, classified := recordedKind(ctx); !classified || kindEmpty(ctx) {
				RecordFailure(ctx, ClassifyFailure(err))
			}
			// The error ends the stream, and it is delivered by the caller of driveTurn —
			// exactly once. Yielding it here as well would call the consumer's callback a
			// second time after it already returned false, which range-over-func panics on.
			return outcome, records, err
		}
		switch typed := event.(type) {
		case Status:
			outcome.State = typed.State
			completed = typed.State.Terminal()
			records = append(records, recordOf(recordState, typed))
		case Text:
			outcome.Content = appendContent(outcome.Content, typed.Text)
		case UsageEvent:
			addUsage(&outcome.Usage, typed.Usage)
		case Interrupted:
			records = append(records, recordOf(recordInterrupted, typed.Interruption))
			annotateInterruption(ctx, typed.Interruption)
		case ArtifactWritten:
			records = append(records, recordOf(recordArtifact, typed.Artifact))
		}
		if !yield(event, nil) {
			// The consumer stopped early. That is a cancellation, not a failure: the
			// caller decided it had enough — so the driver is told to stop too.
			cancel()
			RecordFailure(ctx, FailureCanceled)
			outcome.Err = context.Canceled
			return outcome, records, nil
		}
	}
	if !completed && outcome.State == "" {
		// A driver that ended without a terminal status left the task in an unknown
		// state. Reporting it as failed is the honest reading: the caller cannot resume
		// something that never said where it stopped.
		outcome.State = StateFailed
	}
	return outcome, records, nil
}

// annotateInterruption puts one applied operator input on the turn's span.
//
// The audit half is the checkpoint record beside it: a span event is what a trace reader sees,
// the record is what survives the process. Neither carries the operator's text — the intent and
// the position are what explain the turn's change of direction.
func annotateInterruption(ctx context.Context, interruption Interruption) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.AddEvent(eventInterrupted, trace.WithAttributes(
		attribute.String(telemetry.AttrAgentInterrupted, string(interruption.Intent)),
		attribute.Int(telemetry.AttrAgentBoundary, interruption.Boundary),
	))
}

// appendContent adds one delta to the turn's answer.
func appendContent(content message.Content, text string) message.Content {
	if text == "" {
		return content
	}
	if said := content.Text(); said != "" {
		return message.NewTextContent(said + text)
	}
	return message.NewTextContent(text)
}

// recordOf renders one fact for the event log. A payload that cannot encode is dropped
// rather than failing the turn: the log is evidence, not the result.
func recordOf(kind string, value any) EventRecord {
	payload, err := json.Marshal(value)
	if err != nil {
		return EventRecord{}
	}
	return EventRecord{Kind: kind, Payload: payload}
}

// finish saves the terminal checkpoint, runs the after-turn hooks and closes the span.
//
// It runs on every path out of RunTurn — a driver error, a panic, a consumer that stopped
// — so the record of a turn never depends on which of them happened.
func finish(ctx context.Context, span trace.Span, req Request, turn TurnInfo, outcome TurnOutcome, records []EventRecord, elapsed time.Duration) {
	if outcome.State != "" {
		span.SetAttributes(attribute.String(telemetry.AttrAgentState, string(outcome.State)))
	}
	if outcome.Err != nil {
		kind, recorded := recordedKind(ctx)
		if !recorded || kind == "" {
			kind = FailureUnclassified
			RecordFailure(ctx, kind)
		}
		// The trace carries the classification, not the message: an error string has
		// variable parts (paths, ids, provider text) and belongs in the log.
		span.SetAttributes(attribute.String(telemetry.AttrErrorKind, string(kind)))
		span.SetStatus(codes.Error, string(kind))
		telemetry.WarnErr(ctx, "agent: the turn failed", outcome.Err)
	} else {
		span.SetStatus(codes.Ok, "")
	}
	recordTurnMetrics(ctx, req, outcome)
	recordTurnDuration(ctx, req, elapsed)
	recordTurnUsage(ctx, req, outcome)
	saveTerminal(ctx, req, outcome, records)
	if req.Hooks.AfterTurn != nil {
		if err := req.Hooks.AfterTurn(ctx, turn, outcome); err != nil {
			telemetry.WarnErr(ctx, "agent: an after-turn hook failed", err)
		}
	}
}

// saveTerminal writes where the task stands and the facts that explain it, under the span that
// makes the write visible. A turn with no store keeps nothing, which is what a stateless
// deployment wants.
//
// It writes on a context that ignores cancellation: the decorator cancels the turn's
// context the moment the caller stops or the driver panics, and a store that honoured
// that cancellation would lose the very record explaining why the turn ended. Durability
// is the one thing that must outlive the decision to stop.
func saveTerminal(ctx context.Context, req Request, outcome TurnOutcome, records []EventRecord) {
	if req.Store == nil || req.Task.ID == "" {
		return
	}
	task := req.Task
	if outcome.State != "" {
		task.State = outcome.State
	}
	kept := make([]EventRecord, 0, len(records))
	for _, record := range records {
		if record.Kind != "" {
			kept = append(kept, record)
		}
	}
	saveCtx, end := startSaveSpan(ctx, req, len(kept))
	_, _, err := req.Store.Save(context.WithoutCancel(saveCtx), Checkpoint{
		Schema:      Schema,
		Fingerprint: req.Fingerprint,
		Task:        task,
	}, kept)
	if err != nil {
		telemetry.WarnErr(ctx, "agent: saving the terminal checkpoint failed", err)
	}
	end(err)
}

// startSaveSpan opens the span one terminal save writes under.
//
// It is gated at "run": a deployment that records nothing inside its turns promised two spans
// per turn, and the save is inside the turn. The closer is always safe to call, so the caller
// does not branch on the level.
func startSaveSpan(ctx context.Context, req Request, events int) (context.Context, func(error)) {
	if !req.Trace.AtLeast(TraceRun) {
		return ctx, func(error) {}
	}
	ctx, span := telemetry.TracerWithSuffix("agent").Start(ctx, spanCheckpointSave,
		trace.WithAttributes(attribute.Int(telemetry.AttrCheckpointEvents, events)))
	span.SetAttributes(telemetry.Attrs(ctx)...)
	return ctx, func(err error) {
		if err != nil {
			// The classification goes on the span, the message in the log beside it.
			span.SetStatus(codes.Error, "save_failed")
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}

// startTurnSpan attaches the turn identity and opens the span. The identity is attached
// by the layer that counted the ordinal, which is the host: a driver sees one run and
// cannot know how many its session has had.
func startTurnSpan(ctx context.Context, req Request) (context.Context, trace.Span) {
	if req.Ordinal > 0 {
		attached, err := inject.Turn(ctx, identity.Turn{TaskID: req.Task.ID, Ordinal: req.Ordinal})
		if err != nil {
			telemetry.WarnErr(ctx, "agent: attaching turn identity failed", err)
		} else {
			ctx = attached
		}
	} else {
		telemetry.Warn(ctx, "agent: RunTurn called without an ordinal; the span cannot say which turn it is")
	}
	ctx, span := telemetry.TracerWithSuffix("agent").Start(ctx, spanTurn,
		trace.WithAttributes(
			attribute.Int(telemetry.AttrAgentRound, req.Ordinal),
			attribute.String(telemetry.AttrAgentTraceLevel, req.Trace.String()),
		))
	span.SetAttributes(telemetry.Attrs(ctx)...)
	return ctx, span
}

// kindEmpty reports whether the box exists but nothing classified yet, which is the
// state a driver error arrives in.
func kindEmpty(ctx context.Context) bool {
	kind, _ := recordedKind(ctx)
	return kind == ""
}

// sessionID is the binding's session id, empty when the host bound no session.
func sessionID(b tool.Binding) string {
	if b.Session == nil {
		return ""
	}
	return b.Session.ID()
}

// storeSaver is the narrow view of a store, so a driver cannot compact the log, delete a
// task or rewrite another one's blob.
type storeSaver struct{ store Store }

func (s storeSaver) Append(ctx context.Context, taskID string, events []EventRecord) (int64, int64, error) {
	task := Task{ID: taskID}
	first, last, err := s.store.Save(ctx, Checkpoint{Schema: Schema, Task: task}, events)
	return first, last, err
}

func (s storeSaver) Load(ctx context.Context, taskID string) (Checkpoint, error) {
	return s.store.Load(ctx, taskID)
}
