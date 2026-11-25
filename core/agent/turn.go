package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"runtime/debug"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/identity/inject"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
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

// eventCanceled is the span event a turn that ended because someone stopped it adds.
//
// The cancel itself is recorded by the host, on a span of its own: it happens on the
// caller's goroutine, and the turn's span belongs to the goroutine that is being stopped.
// This event is what joins the two — the turn span says *why* it ended, and the operator's
// action has its own footprint in the trace that asked for it.
const eventCanceled = "agent.canceled"

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
	// recordMessage is one message of the turn's transcript. The two the decorator writes
	// are the input the turn was given and the answer it produced; a driver that owns its
	// own transcript (the graph) keeps a richer copy in its section, and this is what makes
	// the log readable — and resumable — without importing that driver's vocabulary.
	recordMessage = "message"
)

// turnFacts is what the fold accumulated for the terminal save.
//
// It is one value rather than three returns because every field is written by the same
// loop over the same stream, and the store takes them together.
type turnFacts struct {
	outcome   TurnOutcome
	records   []EventRecord
	artifacts []Artifact
	// text holds the answer's deltas as they arrive. Concatenating per delta re-copies the whole
	// answer each time, which is quadratic in the number of deltas on the path where latency *is*
	// the product; the answer is materialised once, in [finish].
	text []string
}

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
		// The level travels from here, not from each producer: a layer that opens a span
		// asks telemetry.Start whether its own floor is met, and this is the only place
		// that knows the deployment's answer.
		ctx = telemetry.WithTrace(ctx, req.Trace)
		// The turn owns the driver's lifetime: when the consumer stops early, nothing
		// else will tell the driver to stop.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		ctx, span := startTurnSpan(ctx, req)
		defer span.End()

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
				finish(ctx, span, req, d, turn, turnFacts{
					outcome: TurnOutcome{Err: err},
				}, started)
				yield(nil, err)
				return
			}
			req.Input = turn.Message
		}

		facts, failure := driveTurn(ctx, cancel, req, d, yield)
		finish(ctx, span, req, d, turn, facts, started)
		if failure != nil {
			yield(nil, failure)
		}
	}
}

// driveTurn runs the driver and folds its stream into the turn's outcome, the facts worth
// persisting, and — when the driver panicked — the error the caller sees instead.
//
// The fold is why this loop reads every event: the terminal state, the usage totals, the
// artifacts and the interruptions are what the checkpoint and the record need, and only a
// reader that saw the whole stream can report them.
func driveTurn(ctx context.Context, cancel context.CancelFunc, req Request, d Driver, yield func(Event, error) bool) (facts turnFacts, failure error) {
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
		facts.outcome.Err = fmt.Errorf("driver panicked: %v", recovered)
		facts.outcome.State = StateFailed
		failure = facts.outcome.Err
		telemetry.Warn(ctx, "agent: the driver panicked",
			attribute.String(telemetry.AttrErrorMessage, fmt.Sprint(recovered)),
			attribute.String(telemetry.AttrCodeStacktrace, string(debug.Stack())))
	}()

	for event, err := range d.Run(ctx, req) {
		if err != nil {
			facts.outcome.Err = err
			if _, classified := recordedKind(ctx); !classified || kindEmpty(ctx) {
				RecordFailure(ctx, ClassifyFailure(err))
			}
			// The error ends the stream, and it is delivered by the caller of driveTurn —
			// exactly once. Yielding it here as well would call the consumer's callback a
			// second time after it already returned false, which range-over-func panics on.
			return facts, err
		}
		switch typed := event.(type) {
		case Status:
			facts.outcome.State = typed.State
			completed = typed.State.Terminal()
			facts.records = append(facts.records, recordOf(recordState, typed))
		case Text:
			if typed.Text != "" {
				facts.text = append(facts.text, typed.Text)
			}
		case UsageEvent:
			addUsage(&facts.outcome.Usage, typed.Usage)
		case Interrupted:
			facts.records = append(facts.records, recordOf(recordInterrupted, typed.Interruption))
			annotateInterruption(ctx, typed.Interruption)
		case ArtifactWritten:
			facts.records = append(facts.records, recordOf(recordArtifact, typed.Artifact))
			facts.artifacts = mergeArtifacts(facts.artifacts, typed.Artifact)
		}
		if !yield(event, nil) {
			// The consumer stopped early. That is a cancellation, not a failure: the
			// caller decided it had enough — so the driver is told to stop too.
			cancel()
			RecordFailure(ctx, FailureCanceled)
			facts.outcome.Err = context.Canceled
			return facts, nil
		}
	}
	if !completed && facts.outcome.State == "" {
		// A driver that ended without a terminal status left the task in an unknown
		// state. Reporting it as failed is the honest reading: the caller cannot resume
		// something that never said where it stopped.
		facts.outcome.State = StateFailed
	}
	return facts, nil
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

// recordOf renders one fact for the event log. A payload that cannot encode is dropped
// rather than failing the turn: the log is evidence, not the result.
func recordOf(kind string, value any) EventRecord {
	payload, err := json.Marshal(value)
	if err != nil {
		return EventRecord{}
	}
	return EventRecord{Kind: kind, Payload: payload}
}

// mergeArtifacts adds one produced artifact to the list, replacing a repeat of the same id.
//
// A driver may announce the same blob twice (a resumed run republishing its output), and a
// projection that grew a duplicate per turn would be wrong in a way nobody notices.
func mergeArtifacts(into []Artifact, artifact Artifact) []Artifact {
	if artifact.ID == "" && artifact.Blob == "" {
		return into
	}
	for i := range into {
		if into[i].ID == artifact.ID && into[i].Blob == artifact.Blob {
			into[i] = artifact
			return into
		}
	}
	return append(into, artifact)
}

// finish saves the terminal checkpoint, runs the after-turn hooks, hands the run to the
// host's sink and closes the span.
//
// It runs on every path out of RunTurn — a driver error, a panic, a consumer that stopped
// — so the record of a turn never depends on which of them happened.
func finish(ctx context.Context, span trace.Span, req Request, d Driver, turn TurnInfo, facts turnFacts, started time.Time) {
	if len(facts.text) > 0 {
		// One allocation for the whole answer, sized exactly: the deltas were kept rather than
		// concatenated so a long stream does not copy itself once per delta.
		facts.outcome.Content = message.NewTextContent(strings.Join(facts.text, ""))
	}
	outcome := facts.outcome
	elapsed := time.Since(started)
	if outcome.State != "" {
		span.SetAttributes(attribute.String(telemetry.AttrAgentState, string(outcome.State)))
	}
	kind := FailureKind("")
	if outcome.Err != nil {
		recorded, ok := recordedKind(ctx)
		if !ok || recorded == "" {
			kind = FailureUnclassified
			RecordFailure(ctx, kind)
		} else {
			kind = recorded
		}
		// The trace carries the classification, not the message: an error string has
		// variable parts (paths, ids, provider text) and belongs in the log.
		span.SetAttributes(attribute.String(telemetry.AttrErrorKind, string(kind)))
		span.SetStatus(codes.Error, string(kind))
		telemetry.WarnErr(ctx, "agent: the turn failed", outcome.Err)
		if kind == FailureCanceled && span.IsRecording() {
			// The same fact the host's cancel span records, from the side that was
			// stopped: one trace says who asked and the other says what the run did.
			span.AddEvent(eventCanceled, trace.WithAttributes(
				attribute.String(telemetry.AttrErrorKind, string(kind))))
		}
	} else {
		span.SetStatus(codes.Ok, "")
	}
	recordTurnMetrics(ctx, req, outcome)
	recordTurnDuration(ctx, req, elapsed)
	recordTurnUsage(ctx, req, outcome)
	saveTerminal(ctx, req, d, facts)
	reportRun(ctx, span, req, outcome, kind, started, elapsed)
	if req.Hooks.AfterTurn != nil {
		if err := req.Hooks.AfterTurn(ctx, turn, outcome); err != nil {
			telemetry.WarnErr(ctx, "agent: an after-turn hook failed", err)
		}
	}
}

// reportRun hands one finished run to the host's sink, when it installed one.
//
// It is the join point between a trace and a dataset row: the ids identify the span, the
// usage totals make cost comparable across runs, and the classification is the same one the
// span carries so the two cannot disagree. The sink is process-owned and called on the
// turn's goroutine — a sink that blocks delays the caller's last event, which is why the
// host is expected to hand over a cheap one.
func reportRun(ctx context.Context, span trace.Span, req Request, outcome TurnOutcome, kind FailureKind, started time.Time, elapsed time.Duration) {
	if req.OnRun == nil {
		return
	}
	record := RunRecord{
		Agent:     req.Agent,
		Manifest:  req.Manifest.ID(),
		TaskID:    req.Task.ID,
		State:     outcome.State,
		ErrorKind: string(kind),
		StartedAt: started,
		LatencyMs: elapsed.Milliseconds(),
		Usage:     outcome.Usage,
	}
	if sc := span.SpanContext(); sc.IsValid() {
		record.TraceID = sc.TraceID().String()
		record.SpanID = sc.SpanID().String()
	}
	record.SessionID = sessionID(req.Binding)
	if run, ok := identity.Lookup(ctx); ok {
		record.TenantID = run.Tenant
	}
	req.OnRun(record)
}

// saveTerminal writes where the task stands and the facts that explain it, under the span that
// makes the write visible. A turn with no store keeps nothing, which is what a stateless
// deployment wants.
//
// It writes on a context that ignores cancellation: the decorator cancels the turn's
// context the moment the caller stops or the driver panics, and a store that honoured
// that cancellation would lose the very record explaining why the turn ended. Durability
// is the one thing that must outlive the decision to stop.
func saveTerminal(ctx context.Context, req Request, d Driver, facts turnFacts) {
	if req.Store == nil || req.Task.ID == "" {
		return
	}
	task := req.Task
	if facts.outcome.State != "" {
		task.State = facts.outcome.State
	}
	task.Round = roundOf(task.Round, d)
	task.Artifacts = carriedArtifacts(task.Artifacts, facts.artifacts)
	kept := make([]EventRecord, 0, len(facts.records)+2)
	for _, record := range facts.records {
		if record.Kind != "" {
			kept = append(kept, record)
		}
	}
	kept = append(kept, transcriptRecords(req, facts.outcome)...)

	saveCtx, span := telemetry.Start(ctx, telemetry.TracerWithSuffix("agent"), spanCheckpointSave,
		telemetry.TraceRun, trace.WithAttributes(attribute.Int(telemetry.AttrCheckpointEvents, len(kept))))
	span.SetAttributes(telemetry.Attrs(saveCtx)...)
	_, _, err := req.Store.Save(context.WithoutCancel(saveCtx), Checkpoint{
		Schema:      Schema,
		Fingerprint: req.Fingerprint,
		Task:        task,
	}, kept)
	if err != nil {
		// The classification goes on the span, the message in the log beside it.
		span.SetStatus(codes.Error, "save_failed")
		telemetry.WarnErr(ctx, "agent: saving the terminal checkpoint failed", err)
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
	recordCheckpointSave(ctx, req.Agent, len(kept), err)
}

// transcriptRecords renders the turn's own two transcript facts: the message it was given and
// the answer it produced.
//
// They are written here, not by the driver, because they are the one part of the transcript
// every driver has in common — a graph keeps a richer copy in its section, a proxy keeps
// none — and a log that only the graph can explain is not a log of the deployment.
//
// Two budgets apply, and both exist to keep a record a *fact* rather than a payload:
//
//   - a message that carries an inline media payload is not written. A pasted image is
//     megabytes of base64, and the event log is a sequence of small facts; a message whose
//     media is a URL is a reference and is written like any other.
//   - the answer is gated on Bodies, which already means "we keep the model's answer".
//     Writing it unconditionally would be the same retention decision made twice, with the
//     quieter of the two winning.
func transcriptRecords(req Request, outcome TurnOutcome) []EventRecord {
	records := make([]EventRecord, 0, 2)
	if (req.Input.Role != "" || len(req.Input.Content.Parts) > 0) && !carriesInlinePayload(req.Input) {
		if record := recordOf(recordMessage, req.Input); record.Kind != "" {
			records = append(records, record)
		}
	}
	if !req.Bodies {
		return records
	}
	said := outcome.Content.Text()
	if said == "" {
		return records
	}
	if record := recordOf(recordMessage, message.NewTextMessage(message.RoleAssistant, said)); record.Kind != "" {
		records = append(records, record)
	}
	return records
}

// carriesInlinePayload reports whether a message holds bytes rather than a reference.
func carriesInlinePayload(msg message.Message) bool {
	for _, part := range msg.Content.Parts {
		switch typed := part.(type) {
		case message.ImagePart:
			if typed.Source.Kind() == media.SourceInline {
				return true
			}
		case message.AudioPart:
			if typed.Source.Kind() == media.SourceInline {
				return true
			}
		case message.VideoPart:
			if typed.Source.Kind() == media.SourceInline {
				return true
			}
		case message.FilePart:
			if typed.Source.Kind() == media.SourceInline {
				return true
			}
		}
	}
	return false
}

// roundOf answers how far the task has got: the driver's own count when it tracks one, and
// the projection's value otherwise.
//
// A driver that steps (the graph) writes its own projection every step and the decorator's
// answer is the same one; a driver that does not (a proxy) has no other way to say it, and
// leaving the field at zero made the list projection incomparable between two deployments
// that both have a "round" column.
func roundOf(known int, d Driver) int {
	rounder, ok := d.(Rounder)
	if !ok {
		return known
	}
	if round := rounder.Round(); round > known {
		return round
	}
	return known
}

// carriedArtifacts keeps what the projection already named and adds what this turn produced.
func carriedArtifacts(known, produced []Artifact) []Artifact {
	merged := known
	for _, artifact := range produced {
		merged = mergeArtifacts(merged, artifact)
	}
	return merged
}

// startTurnSpan attaches the turn identity and opens the span. The identity is attached
// by the layer that counted the ordinal, which is the host: a driver sees one run and
// cannot know how many its session has had.
//
// The span has no floor: a turn boundary is what the deployment asked for even at the
// lowest level, which is why TraceNone is "the run and the turn".
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
	ctx, span := telemetry.Start(ctx, telemetry.TracerWithSuffix("agent"), spanTurn, telemetry.TraceNone,
		trace.WithAttributes(
			attribute.Int(telemetry.AttrAgentRound, req.Ordinal),
			attribute.String(telemetry.AttrAgentTraceLevel, req.Trace.String()),
		))
	span.SetAttributes(telemetry.AttrsFor(span, ctx)...)
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
