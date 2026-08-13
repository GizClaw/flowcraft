package agent

import "context"

// Engine is the deliberately thin contract every local execution
// engine satisfies so the agent layer can drive it through a uniform
// shape. Concrete engines (core/graph DAG executor, future script-
// based engines, …) usually expose richer APIs in addition to this
// method.
//
// Contract:
//
//   - Execute MUST run to completion, until interrupted, or until
//     ctx is cancelled.
//
//   - On clean completion, return the final Board (often the same
//     pointer as the input — engines mutate in place by default) and
//     a nil error.
//
//   - On cooperative interrupt (host sent through host.Interrupts()),
//     return the (partial) Board together with the result of
//     [Interrupted]. The error then satisfies
//     errdefs.IsInterrupted(err) and can be destructured into an
//     [InterruptedError] for the cause.
//
//   - On ctx cancellation, return the (partial) Board and ctx.Err().
//
//   - On any other failure, return the (partial) Board together with
//     a domain error (preferably classified via errdefs). Returning a
//     non-nil board on error lets the host decide whether to commit /
//     discard / persist.
//
//   - When run.ResumeFrom is non-nil, Execute resumes from that
//     checkpoint instead of starting fresh. See [Run.ResumeFrom] for
//     the resume contract; engines that do not support resume MUST
//     return an errdefs.NotAvailable-classified error rather than
//     silently restarting.
//
// Engines MUST NOT close any host-owned channel and MUST NOT mutate
// run.Attributes or run.ResumeFrom.
type Engine interface {
	Execute(ctx context.Context, run Run, host Host, board *Board) (*Board, error)
}

// EngineFunc adapts a plain function to the [Engine] interface.
// Useful for test doubles and trivial engines.
type EngineFunc func(ctx context.Context, run Run, host Host, board *Board) (*Board, error)

// Execute satisfies [Engine].
func (f EngineFunc) Execute(ctx context.Context, run Run, host Host, board *Board) (*Board, error) {
	if f == nil {
		return board, nil
	}
	return f(ctx, run, host, board)
}

// ---------- Capabilities ----------

// Capabilities describes the optional features an engine kind
// declares to its host. The declaration is made once, statically,
// when the engine kind is registered — engine factories expose it
// through a capability interface that core/deploy and hosts assert —
// never probed per instance at run time. Hosts read capabilities
// to:
//
//   - validate a deployment spec at Apply time (e.g. a restart
//     policy requiring resume is rejected when the engine kind does
//     not claim SupportsResume);
//   - decide which host capabilities to wire (attach a
//     CheckpointStore only when EmitsCheckpoint is claimed);
//   - refuse to run an engine that needs user prompts in a headless
//     deployment (EmitsUserPrompt=true on a host without an
//     interactive channel becomes a fail-fast).
//
// All fields default to "do not claim the capability" (zero value).
type Capabilities struct {
	// SupportsResume reports whether Execute can honour
	// Run.ResumeFrom. Engines that always return errdefs.NotAvailable
	// for a non-nil ResumeFrom MUST leave this false; deployments
	// enforcing a restart-always policy need the true case to recover
	// mid-run state without losing partial work.
	SupportsResume bool `json:"supports_resume,omitempty" yaml:"supports_resume,omitempty"`

	// EmitsUserPrompt reports whether the engine may call
	// Host.AskUser during Execute. Hosts deploying in headless /
	// batch mode use this to refuse engines that would block waiting
	// for a reply that nobody can supply.
	EmitsUserPrompt bool `json:"emits_user_prompt,omitempty" yaml:"emits_user_prompt,omitempty"`

	// EmitsCheckpoint reports whether the engine writes Checkpoints
	// during Execute (independently of SupportsResume — an engine
	// can write checkpoints that only an external tool can replay).
	// Hosts use this to decide whether to attach a CheckpointStore.
	EmitsCheckpoint bool `json:"emits_checkpoint,omitempty" yaml:"emits_checkpoint,omitempty"`
}

// CheckpointSuggester is the optional engine-side interface a host
// uses to ask the engine to write a Checkpoint at the next safe
// boundary — typically before a voluntary restart, scale-down, or
// reschedule.
//
// Semantics (advisory, not synchronous):
//
//   - The engine SHOULD call its host's Checkpointer at the next
//     point in execution where Checkpoint.Steps is well-defined. It is
//     NOT obligated to interrupt itself; SuggestCheckpoint returns
//     immediately with no guarantee that the checkpoint has been
//     written by the time it returns.
//   - The host typically pairs SuggestCheckpoint with a follow-up
//     Interrupt after a grace period: "save what you can, then stop".
//   - Engines that have no notion of a step boundary (purely
//     memory-resident, sub-second runs) MAY treat this as a no-op.
//
// Unlike [Capabilities] this is a *behavioural* interface — the
// engine has to do something — so it stays an interface assertion
// rather than a spec field.
type CheckpointSuggester interface {
	SuggestCheckpoint() error
}

// SuggestCheckpoint asks the engine for a voluntary checkpoint when
// the engine implements [CheckpointSuggester]; otherwise it is a
// no-op. Returns the engine's error directly so the host can log /
// retry; nil is returned both for "engine does not support
// suggestion" and for "engine accepted the suggestion".
func SuggestCheckpoint(e Engine) error {
	if s, ok := e.(CheckpointSuggester); ok {
		return s.SuggestCheckpoint()
	}
	return nil
}
