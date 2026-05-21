// Package pipeline is the v2 recall stage-orchestration framework.
//
// Every recall flow (write, read, rebuild) is expressed as an
// ordered slice of Stages over a flow-specific State pointer. The
// framework owns wall-clock measurement, StageDiagnostic emission,
// short-circuit and compensation semantics, so individual stages
// stay narrow:
//
//	type Stage[S any] interface {
//	    Name() string
//	    Run(ctx context.Context, state S) (diagnostic.StageDetail, error)
//	}
//
// State is the only data channel between stages — context values
// are forbidden so stages stay testable in isolation. Each Run call
// returns its strongly-typed StageDetail; the framework wraps it in
// a StageDiagnostic, appends to the flow's trace via the caller-
// supplied TraceAppender, and pushes it through the optional
// TelemetryHook. There is no other emission path.
package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Stage is one ordered step of a recall pipeline. Implementations
// stay narrow: read inputs from state, mutate state with their
// output, return a strongly-typed StageDetail describing what
// happened. Telemetry / trace emission is the framework's
// responsibility — stages must NOT call hooks directly.
//
// Run may return a sentinel ShortCircuit error (see shortcircuit.go)
// to terminate the pipeline successfully without invoking later
// stages or compensators. Any other non-nil error triggers reverse-
// order compensation across already-executed stages.
type Stage[S any] interface {
	Name() string
	Run(ctx context.Context, state S) (diagnostic.StageDetail, error)
}

// Compensator is an optional companion contract a Stage may
// implement to roll back its own side effects when a downstream
// stage fails. The framework invokes Compensate in reverse order
// across the stages that already ran successfully, using a
// context detached from the inbound cancellation so cleanup
// always reaches completion (see compensator.go).
//
// Compensation only runs after a real Stage error — ShortCircuit
// is a normal exit and never triggers it. A compensator that
// itself returns a non-nil error is reported via the telemetry
// hook (CompensationFailedDetail); the framework still proceeds
// with earlier-stage compensators so a single failing rollback
// does not strand the remainder.
type Compensator[S any] interface {
	Compensate(ctx context.Context, state S) error
}

// Conditional is an optional companion contract that lets a Stage
// declare it should be skipped under current state. If Skip returns
// true the framework emits a Status=Skipped StageDiagnostic with
// the returned Detail (so observers see why the slot was empty)
// and moves on without invoking Run.
//
// Skip's Detail value is the same shape Run would have returned —
// dashboards key off Stage name + Status, the Detail discriminates
// the "why".
type Conditional[S any] interface {
	Skip(ctx context.Context, state S) (skip bool, detail diagnostic.StageDetail)
}

// TraceAppender is how Pipeline writes StageDiagnostics back to the
// State's flow-specific trace surface (RecallTrace.Stages /
// SaveTrace.Stages / RebuildTrace.Stages). The runner supplies
// this callback at Pipeline construction so the framework stays
// agnostic to the trace's concrete container type.
//
// A nil TraceAppender is valid — useful for tests that only care
// about TelemetryHook emission — and means Run discards diagnostics.
type TraceAppender[S any] func(state S, diag diagnostic.StageDiagnostic)

// Pipeline executes Stages in order and owns the diagnostic
// emission contract for one recall flow. It is parameterised over
// a state pointer S (typically *WriteState / *ReadState /
// *RebuildState) so the same framework drives every phase without
// any reflection or empty interfaces.
//
// The zero Pipeline is valid (no stages, no telemetry, no trace
// appender) and Run on it is a successful no-op. Callers
// construct one via NewPipeline or by assembling the struct
// directly — the latter is the path the per-flow Runners take
// because they configure all three knobs at once.
type Pipeline[S any] struct {
	// Phase tags every emitted StageDiagnostic so subscribers can
	// route by write / read / rebuild without inspecting Stage
	// names.
	Phase diagnostic.Phase

	// Stages executed in order. Empty slice = no-op Run.
	Stages []Stage[S]

	// Telemetry receives one OnStage event per emitted
	// StageDiagnostic. nil is permitted: the framework checks
	// before invoking it. Emission ordering matches trace
	// append order — subscribers can rebuild trace from the event
	// stream alone.
	Telemetry port.TelemetryHook

	// AppendTrace writes a finished StageDiagnostic into the
	// state's flow-specific trace slot. nil discards diagnostics
	// (Telemetry can still observe them).
	AppendTrace TraceAppender[S]
}

// NewPipeline constructs a Pipeline with the supplied phase, stage
// list, telemetry hook, and trace appender. Either of the last
// two may be nil; the Stages slice may be empty and Run on the
// result returns nil.
func NewPipeline[S any](phase diagnostic.Phase, stages []Stage[S], hook port.TelemetryHook, appender TraceAppender[S]) *Pipeline[S] {
	return &Pipeline[S]{
		Phase:       phase,
		Stages:      stages,
		Telemetry:   hook,
		AppendTrace: appender,
	}
}

// Run executes every Stage in order and returns the first non-
// ShortCircuit error encountered. The returned error preserves
// the original cause so caller-side errors.Is / errors.As keep
// working — the framework wraps nothing.
//
// Lifecycle per stage:
//
//  1. If the stage implements Conditional and Skip returns true,
//     emit a Status=Skipped StageDiagnostic with the returned
//     Detail and move on. The stage is NOT recorded as executed
//     so its Compensator (if any) will not be invoked on later
//     failure.
//
//  2. Otherwise call Run. Build a StageDiagnostic capturing wall
//     clock, returned Detail, error, and ErrClass; append to trace
//     and emit via telemetry.
//
//  3. On a ShortCircuit sentinel: stop iteration, return nil.
//     Already-executed stages keep their Status=ok. The
//     short-circuiting stage's own diagnostic carries
//     Status=short_circuit and Reason in Err. No diagnostics are
//     emitted for the remaining unrun stages — they did not run.
//
//  4. On any other error: emit the failed diagnostic, then walk
//     the executed-stage history in reverse and Compensate any
//     stage that implements Compensator. Each compensated stage's
//     original diagnostic is re-emitted with Status=compensated.
//     If a compensator itself fails, the framework emits a
//     CompensationFailedDetail event and proceeds to the next
//     earlier compensator. The original Run error is returned.
//
//  5. On success: emit Status=ok and record the stage as
//     executed.
func (p *Pipeline[S]) Run(ctx context.Context, state S) error {
	executed := make([]int, 0, len(p.Stages))
	for i, stage := range p.Stages {
		if cond, ok := stage.(Conditional[S]); ok {
			if skip, detail := cond.Skip(ctx, state); skip {
				p.emit(state, diagnostic.StageDiagnostic{
					Stage:   stage.Name(),
					Phase:   p.Phase,
					Order:   i,
					StartAt: time.Now(),
					Status:  diagnostic.StatusSkipped,
					Detail:  detail,
				})
				continue
			}
		}

		started := time.Now()
		detail, err := stage.Run(ctx, state)
		diag := diagnostic.StageDiagnostic{
			Stage:    stage.Name(),
			Phase:    p.Phase,
			Order:    i,
			StartAt:  started,
			Duration: time.Since(started),
			Detail:   detail,
		}

		var sc ShortCircuit
		if errors.As(err, &sc) {
			diag.Status = diagnostic.StatusShortCircuit
			diag.Err = sc.Reason
			p.emit(state, diag)
			return nil
		}

		var bef BestEffortFailure
		if errors.As(err, &bef) {
			diag.Status = diagnostic.StatusDegraded
			diag.Err = bef.Err.Error()
			p.emit(state, diag)
			executed = append(executed, i)
			continue
		}

		if err != nil {
			diag.Status = diagnostic.StatusFailed
			diag.Err = err.Error()
			p.emit(state, diag)
			p.runCompensators(ctx, state, executed, err)
			return err
		}

		diag.Status = diagnostic.StatusOK
		p.emit(state, diag)
		executed = append(executed, i)
	}
	return nil
}

// emit appends to trace and pushes to the telemetry hook in that
// order. Both targets are optional; missing ones are silently
// skipped. Emission order matches the per-stage call order so
// subscribers can reconstruct trace from events alone.
func (p *Pipeline[S]) emit(state S, diag diagnostic.StageDiagnostic) {
	if p.AppendTrace != nil {
		p.AppendTrace(state, diag)
	}
	if p.Telemetry != nil {
		p.Telemetry.OnStage(diag)
	}
}

// runCompensators walks executed in reverse, invoking Compensate
// on any stage that implements Compensator. The detached context
// (see DetachCancel) keeps cleanup running even when the inbound
// ctx was cancelled. Compensator errors do not interrupt the
// rollback chain — they are reported via CompensationFailedDetail
// so an operator can see exactly which compensator could not
// finish.
func (p *Pipeline[S]) runCompensators(ctx context.Context, state S, executed []int, cause error) {
	cleanupCtx := DetachCancel(ctx)
	for k := len(executed) - 1; k >= 0; k-- {
		idx := executed[k]
		comp, ok := p.Stages[idx].(Compensator[S])
		if !ok {
			continue
		}
		started := time.Now()
		if err := comp.Compensate(cleanupCtx, state); err != nil {
			p.emit(state, diagnostic.StageDiagnostic{
				Stage:    p.Stages[idx].Name() + ":compensate",
				Phase:    p.Phase,
				Order:    idx,
				StartAt:  started,
				Duration: time.Since(started),
				Status:   diagnostic.StatusFailed,
				Err:      err.Error(),
				Detail: diagnostic.CompensationFailedDetail{
					OriginalStage: p.Stages[idx].Name(),
					Cause:         cause.Error(),
				},
			})
			continue
		}
		p.emit(state, diagnostic.StageDiagnostic{
			Stage:    p.Stages[idx].Name(),
			Phase:    p.Phase,
			Order:    idx,
			StartAt:  started,
			Duration: time.Since(started),
			Status:   diagnostic.StatusCompensated,
		})
	}
}
