package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Resumer is the optional capability an [Engine] may advertise to
// signal it can drive [Run.ResumeFrom] and to short-circuit obvious
// mismatches *before* the host spins up an execution.
//
// The interface is intentionally minimal — a single CanResume probe
// — so engines opt in cheaply and hosts can build resume tooling
// (admin UIs, CLI commands, supervised retry loops) without a full
// dry-run.
//
// Engines that do NOT implement Resumer remain fully spec-compliant:
// the [Run.ResumeFrom] contract still applies and they MUST return
// an [errdefs.NotAvailable]-classified error if asked to resume. The
// helpers below ([IsResumable], [LoadAndResume]) treat the absence
// of Resumer as "engine handles resume opaquely; trust Execute to
// surface the right error".
type Resumer interface {
	// CanResume returns nil if the given checkpoint is resumable by
	// this engine, or a classified error explaining why not:
	//
	//   - errdefs.Validation: checkpoint shape is wrong (engine kind
	//     mismatch, missing required Payload fields, ExecID
	//     conflict).
	//   - errdefs.NotAvailable: engine recognises the checkpoint but
	//     cannot resume it (incompatible engine version, removed
	//     node type, etc.).
	//
	// Implementations MUST be cheap (no I/O, no LLM calls); the
	// probe runs synchronously on the host's resume path.
	CanResume(cp Checkpoint) error
}

// IsResumable reports whether eng implements [Resumer]. Use the
// two-value variant when you want the typed handle:
//
//	if r, ok := engine.AsResumer(eng); ok { _ = r.CanResume(cp) }
func IsResumable(eng Engine) bool {
	_, ok := AsResumer(eng)
	return ok
}

// AsResumer is the typed counterpart of [IsResumable]. It walks any
// [WithCapabilities]-style wrappers via Unwrap (errors.As-style) so
// a Resumer wrapped to advertise additional capabilities still
// surfaces correctly.
func AsResumer(eng Engine) (Resumer, bool) {
	for eng != nil {
		if r, ok := eng.(Resumer); ok {
			return r, true
		}
		u, ok := eng.(interface{ Unwrap() Engine })
		if !ok {
			return nil, false
		}
		eng = u.Unwrap()
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// ResumeContext — per-run resume metadata threaded through context.Context
// ---------------------------------------------------------------------------

// ResumeContext is the auxiliary metadata about the *current* resume
// attempt. Engines, observers and middleware read it from
// context.Context to differentiate fresh starts from resumes, count
// attempts in retry loops, and surface "this is replay" indicators
// in trace UIs.
//
// ResumeContext is distinct from [Run.ResumeFrom]: the checkpoint
// describes WHERE the engine should pick up; ResumeContext describes
// WHY the resume is happening (who triggered it, which attempt this
// is, how long the original run has been alive). Keep them separate
// so hosts can drive replay + retry semantics without mutating the
// checkpoint payload.
type ResumeContext struct {
	// Attempt is 1-based: the very first attempt (fresh run, no
	// resume) is 1; the first re-execution after a checkpoint is
	// 2; and so on. Hosts that limit retry budget read this field.
	Attempt int

	// StartedAt is the wall-clock time the original [Run] began.
	// Stays constant across resumes so dashboards can compute
	// total wall time (e.g. SLO budget burn).
	StartedAt time.Time

	// Signal identifies the trigger that produced THIS resume.
	// Convention (extensible — engines/middleware MUST treat
	// unknown values as opaque):
	//
	//   - "manual"           — operator clicked Resume / CLI
	//   - "interrupt-recovery" — host re-executed after a host.Interrupts() Stop
	//   - "schedule"         — cron / queue replayer
	//   - "crash"            — supervisor restarted after process exit
	//   - "retry"            — automated retry on classified failure
	Signal string

	// CheckpointAt is when the checkpoint that fuels this resume
	// was originally produced (the engine's view of "paused at").
	// Empty time.Time on fresh starts.
	CheckpointAt time.Time
}

type resumeCtxKey struct{}

// WithResumeContext returns a derived context carrying rc. Engines
// that publish telemetry attribs (attempt count, resume signal)
// pull from here instead of plumbing extra parameters.
func WithResumeContext(ctx context.Context, rc ResumeContext) context.Context {
	return context.WithValue(ctx, resumeCtxKey{}, rc)
}

// ResumeContextFromContext returns the ResumeContext attached to
// ctx, plus an "ok" flag. The ok=false branch means the engine is
// running a fresh start (or the host did not bother to populate
// the context — engines should treat both cases identically).
func ResumeContextFromContext(ctx context.Context) (ResumeContext, bool) {
	if ctx == nil {
		return ResumeContext{}, false
	}
	rc, ok := ctx.Value(resumeCtxKey{}).(ResumeContext)
	return rc, ok
}

// ---------------------------------------------------------------------------
// LoadAndResume — high-level helper that wires CheckpointStore + Engine
// ---------------------------------------------------------------------------

// LoadAndResumeOption tunes [LoadAndResume] behaviour.
type LoadAndResumeOption func(*loadAndResumeOpts)

type loadAndResumeOpts struct {
	signal       string
	attempt      int
	startedAt    time.Time
	freshAllowed bool
}

// WithResumeSignal sets [ResumeContext.Signal] for the about-to-run
// execution. Defaults to "manual".
func WithResumeSignal(s string) LoadAndResumeOption {
	return func(o *loadAndResumeOpts) {
		if s != "" {
			o.signal = s
		}
	}
}

// WithResumeAttempt sets [ResumeContext.Attempt]. The default of 1
// is correct on fresh runs; supervisors implementing retry loops
// should bump this for each re-attempt.
func WithResumeAttempt(n int) LoadAndResumeOption {
	return func(o *loadAndResumeOpts) {
		if n > 0 {
			o.attempt = n
		}
	}
}

// WithResumeStartedAt sets [ResumeContext.StartedAt]. Default is
// time.Now() at the moment LoadAndResume is invoked. Pass an
// earlier timestamp when continuing a long-running run so dashboard
// "total wall time" remains accurate.
func WithResumeStartedAt(t time.Time) LoadAndResumeOption {
	return func(o *loadAndResumeOpts) {
		if !t.IsZero() {
			o.startedAt = t
		}
	}
}

// WithFreshStartAllowed controls behaviour when the store has no
// checkpoint for run.ID. Default true — execute fresh. Pass false
// to require an existing checkpoint and surface
// errdefs.NotFound when none is present (useful for "resume only"
// admin commands).
func WithFreshStartAllowed(allowed bool) LoadAndResumeOption {
	return func(o *loadAndResumeOpts) {
		o.freshAllowed = allowed
	}
}

// LoadAndResume is the canonical host-side helper for "either
// continue the existing run or start fresh". It:
//
//  1. Loads the most recent checkpoint for run.ID from store.
//  2. If a checkpoint exists, validates it against the engine's
//     [Resumer] (if implemented) — invalid checkpoints surface
//     immediately rather than after a partial Execute.
//  3. Populates run.ResumeFrom and threads a [ResumeContext] onto
//     ctx so the engine, observers and middleware see consistent
//     replay metadata.
//  4. Calls eng.Execute and returns its result.
//
// store may be nil; that is treated as "no checkpoints persisted"
// and is equivalent to a fresh-start with WithFreshStartAllowed
// honoured. board is the bootstrap board used on fresh starts and
// when the engine wishes to keep the host-supplied initial state
// (the engine itself decides whether to override with
// ResumeFrom.Board per the [Run.ResumeFrom] contract).
//
// LoadAndResume is intentionally a one-shot helper, not a retry
// loop: a supervisor that wants exponential backoff or budget
// enforcement composes LoadAndResume with its own retry policy.
func LoadAndResume(
	ctx context.Context,
	eng Engine,
	host Host,
	store CheckpointStore,
	run Run,
	board *Board,
	opts ...LoadAndResumeOption,
) (*Board, error) {
	if eng == nil {
		return board, errors.New("engine.LoadAndResume: nil engine")
	}
	if run.ID == "" {
		return board, errdefs.Validation(errors.New("engine.LoadAndResume: Run.ID is required"))
	}

	o := loadAndResumeOpts{signal: "manual", attempt: 1, freshAllowed: true}
	for _, fn := range opts {
		fn(&o)
	}
	if o.startedAt.IsZero() {
		o.startedAt = time.Now()
	}

	var cp *Checkpoint
	if store != nil {
		loaded, err := store.Load(ctx, run.ID)
		if err != nil {
			return board, fmt.Errorf("engine.LoadAndResume: load %s: %w", run.ID, err)
		}
		cp = loaded
	}

	if cp == nil {
		if !o.freshAllowed {
			return board, errdefs.NotFound(fmt.Errorf("engine.LoadAndResume: no checkpoint for run %s", run.ID))
		}
		// Fresh start path. Still populate ResumeContext so
		// observers can record the attempt count uniformly.
		rc := ResumeContext{
			Attempt:   o.attempt,
			StartedAt: o.startedAt,
			Signal:    o.signal,
		}
		return eng.Execute(WithResumeContext(ctx, rc), run, host, board)
	}

	// Resume path — verify the engine recognises the checkpoint
	// before launching Execute. Engines that do not implement
	// Resumer skip the probe and rely on their own Execute-time
	// handling per the [Run.ResumeFrom] contract.
	if cp.ExecID != "" && cp.ExecID != run.ID {
		return board, errdefs.Validation(fmt.Errorf(
			"engine.LoadAndResume: checkpoint exec_id %q does not match run %q",
			cp.ExecID, run.ID,
		))
	}
	if r, ok := AsResumer(eng); ok {
		if err := r.CanResume(*cp); err != nil {
			return board, fmt.Errorf("engine.LoadAndResume: CanResume: %w", err)
		}
	}

	run.ResumeFrom = cp
	// Prefer the checkpoint's OriginalStartedAt so dashboards see
	// one continuous wall-clock run across resume boundaries. Fall
	// back to the caller-supplied o.startedAt for older checkpoints
	// produced before that field existed (zero time means "not
	// recorded" per Checkpoint.OriginalStartedAt godoc).
	startedAt := cp.OriginalStartedAt
	if startedAt.IsZero() {
		startedAt = o.startedAt
	}
	rc := ResumeContext{
		// First resume after a fresh attempt → 2; honour
		// caller-supplied attempt so retry loops can override.
		Attempt:      max2(o.attempt, 2),
		StartedAt:    startedAt,
		Signal:       o.signal,
		CheckpointAt: cp.Timestamp,
	}
	return eng.Execute(WithResumeContext(ctx, rc), run, host, board)
}

// max2 — local helper to keep the import-free profile of this file.
// Go 1.21's built-in max() also works; using a tiny helper keeps the
// minimum supported toolchain consistent with the rest of sdk.
func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}
