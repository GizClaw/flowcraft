package engine

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// Host is the contract a runtime exposes to a running engine.
//
// Host is intentionally a *composition* of small, single-method
// interfaces — Publisher, Interrupter, UserPrompter, Checkpointer.
// The composition exists to keep [Engine.Execute] readable; downstream
// code (graph nodes, tools, …) should depend on the smallest
// interface it actually needs:
//
//	// A pure-mapping node only emits events:
//	func (n *MapNode) Execute(ctx, board, pub engine.Publisher) error
//
//	// A streaming LLM node also wants the interrupt channel:
//	func (n *LLMNode) Execute(ctx, board,
//	    pub engine.Publisher, intr engine.Interrupter) error
//
// Host implementations must be safe for concurrent use. The engine
// may invoke any method from any goroutine.
type Host interface {
	Publisher
	Interrupter
	UserPrompter
	Checkpointer
	UsageReporter
}

// Publisher emits a single event envelope.
//
// Subject schema is NOT owned by this package: the host decides what
// the routing keys look like. Engines simply produce envelopes whose
// subject they construct from whatever convention their host has
// agreed with the consumer side.
//
// Publish errors MUST NOT cause the producing engine to fail the run
// by default; the engine should log/record and continue. Backpressure
// or transport failures are an observability concern, not a control-
// flow signal.
type Publisher interface {
	Publish(ctx context.Context, env event.Envelope) error
}

// Interrupter exposes the host's cooperative-interrupt channel.
//
// Engines select on the returned channel between steps:
//
//	select {
//	case intr := <-h.Interrupts():
//	    return engine.Interrupted(intr)
//	case <-ctx.Done():
//	    return ctx.Err()
//	default:
//	}
//
// A nil channel means "no cooperative interrupt available"; engines
// should treat it as "never fires" — receiving on nil blocks forever,
// which is the correct semantic.
type Interrupter interface {
	Interrupts() <-chan Interrupt
}

// UserPrompter lets an engine ask the host to prompt the end user
// (chat input, voice DTMF, structured form, …) and block until the
// reply arrives.
//
// Hosts that don't expose user interaction should return an
// errdefs.NotAvailable-classified error. Engines that get such an
// error from a step that strictly needs user input should propagate
// it so the host can decide whether to fail or fall back.
type UserPrompter interface {
	AskUser(ctx context.Context, prompt UserPrompt) (UserReply, error)
}

// Checkpointer persists a checkpoint at a safe boundary the engine
// has reached. The host decides whether to actually write; engines
// must not assume durability.
//
// Hosts without configured checkpointing should make this a no-op
// (return nil) rather than an error so engines can call it
// unconditionally.
type Checkpointer interface {
	Checkpoint(ctx context.Context, cp Checkpoint) error
}

// UsageReporter accepts incremental LLM token-usage reports an engine
// observes during a run. Each call adds delta usage; the host is
// responsible for accumulation, billing, and downstream telemetry.
//
// Engines should call ReportUsage once per LLM invocation that
// returns usage metadata (typical: streaming nodes call it on
// completion with the per-call totals). Reports SHOULD be best-effort
// for *observability* failures — a slow exporter must not block forward
// progress.
//
// Budget enforcement contract:
//
//   - The host MAY return errdefs.BudgetExceeded (or any error
//     classified by errdefs.IsBudgetExceeded) to signal that the
//     accumulated usage has crossed a configured budget and the next
//     LLM call would not be authorised.
//   - Engines that observe such an error MUST stop performing further
//     LLM-cost-incurring work in this run and return the error from
//     Execute (typically wrapped). Continuing would defeat the budget.
//   - Any other non-nil error is observability-only — engines SHOULD
//     log/swallow and continue, matching the pre-budget contract.
//
// Hosts without billing or budget enforcement return nil
// unconditionally (see [NoopHost.ReportUsage]).
type UsageReporter interface {
	ReportUsage(ctx context.Context, usage model.TokenUsage) error
}

// NoopHost is a zero-cost Host implementation that discards events,
// never reports interrupts, refuses user prompts, and skips
// checkpoints. It is meant for tests and embedded scenarios where an
// engine is invoked outside any real runtime.
type NoopHost struct{}

// Publish discards the envelope.
func (NoopHost) Publish(context.Context, event.Envelope) error { return nil }

// Interrupts returns nil so engines that select on it block forever
// on that case (i.e. interrupts never fire under NoopHost).
func (NoopHost) Interrupts() <-chan Interrupt { return nil }

// AskUser returns errdefs.NotAvailable so engines can detect that
// user interaction is unsupported in this host.
func (NoopHost) AskUser(context.Context, UserPrompt) (UserReply, error) {
	return UserReply{}, errdefs.NotAvailablef("engine: user prompts are not supported by this host")
}

// Checkpoint discards the checkpoint.
func (NoopHost) Checkpoint(context.Context, Checkpoint) error { return nil }

// ReportUsage discards the usage report. NoopHost has no budget so
// always returns nil — engines never see BudgetExceeded under it.
func (NoopHost) ReportUsage(context.Context, model.TokenUsage) error { return nil }
