package agent

import (
	"context"
	"reflect"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/inference"
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

// EventBusProvider is an optional Host capability for consumers that
// need to subscribe to the same event surface used by [Publisher].
//
// The Host (or its owning engine/runtime) owns the returned bus and its
// lifecycle. Consumers only borrow it: they may subscribe and publish
// through the Host, but must not close the bus.
//
// EventBusProvider is intentionally not part of [Host]. Hosts that only
// publish events do not need to manufacture a subscribable bus, and
// [NoopHost] deliberately does not implement this capability.
type EventBusProvider interface {
	EventBus() event.Bus
}

// HostUnwrapper is the opt-in contract for Host decorators that preserve
// access to optional capabilities on an inner Host.
//
// UnwrapHost is read-only: callers may inspect the returned Host for
// capabilities but do not replace the decorator's inner Host. Decorators must
// return nil when crossing their boundary would bypass an authoritative
// replacement surface. For example, HostFuncs does not unwrap when PublishFn
// replaces the inner publisher.
type HostUnwrapper interface {
	UnwrapHost() Host
}

// CapabilityFromHost finds an optional capability implemented by h or by an
// inner Host explicitly exposed through [HostUnwrapper]. Typed-nil hosts and
// capabilities are treated as unsupported.
//
// Only decorators explicitly implementing HostUnwrapper are traversed. A
// wrapper can stop traversal by returning nil when it replaces the surface
// that authorises access.
func CapabilityFromHost[T any](h Host) (T, bool) {
	var zero T
	const maxWrapperDepth = 64
	for range maxWrapperDepth {
		if isNilInterface(h) {
			return zero, false
		}
		if capability, ok := any(h).(T); ok && !isNilInterface(capability) {
			return capability, true
		}
		unwrapper, ok := h.(HostUnwrapper)
		if !ok || isNilInterface(unwrapper) {
			return zero, false
		}
		h = unwrapper.UnwrapHost()
	}
	return zero, false
}

// EventBusFromHost returns h's borrowed event bus when h implements
// [EventBusProvider]. It treats nil interfaces, typed-nil hosts, and
// typed-nil buses as unsupported. Built-in Host decorators preserve the
// optional capability without claiming EventBusProvider themselves.
func EventBusFromHost(h Host) (event.Bus, bool) {
	provider, ok := CapabilityFromHost[EventBusProvider](h)
	if !ok {
		return nil, false
	}
	bus := provider.EventBus()
	if isNilInterface(bus) {
		return nil, false
	}
	return bus, true
}

func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
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
//
// The usage value is inference.Usage: token totals plus the Model /
// LatencyMs envelope, with cost carried by Billing.Cost (a Money with
// explicit currency and scale) when a pricing catalog is configured.
type UsageReporter interface {
	ReportUsage(ctx context.Context, usage inference.Usage) error
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
func (NoopHost) ReportUsage(context.Context, inference.Usage) error { return nil }

// ---------- Interrupt ----------

// Cause classifies why a run was asked to stop. The agent layer maps
// these onto its higher-level commit/discard policy (e.g. discard the
// partial output on user_input, commit it on host_shutdown).
//
// Engines should NEVER branch on Cause for control flow — Cause is
// metadata for the host, not a directive for the engine. The engine's
// only correct response to any cause is "stop cleanly and return".
type Cause string

const (
	// CauseUnknown is the zero value. Hosts should avoid sending it;
	// it exists so a zero-value Interrupt is recognisable.
	CauseUnknown Cause = ""

	// CauseUserCancel is a user-initiated cancel ("stop talking",
	// "abort this turn"). Output is typically discarded.
	CauseUserCancel Cause = "user_cancel"

	// CauseUserInput is a barge-in: the user spoke / typed and the
	// agent should yield to fresh input. Output is typically discarded.
	CauseUserInput Cause = "user_input"

	// CauseHostShutdown is a graceful shutdown from the host.
	// Output should typically be committed if any was produced.
	CauseHostShutdown Cause = "host_shutdown"

	// CauseCustom carries a host-defined cause in [Interrupt.Detail].
	CauseCustom Cause = "custom"
)

// Interrupt is the value the host sends through Host.Interrupts() to
// ask the running engine to stop. It is also the payload of the
// [Interrupted] error so the host can introspect why.
//
// Interrupt is a plain value — copy it freely.
type Interrupt struct {
	Cause  Cause
	Detail string
}

// Interrupted wraps an [Interrupt] as an error that satisfies
// [errdefs.IsInterrupted]. The recommended usage from an engine is:
//
//	case intr := <-h.Interrupts():
//	    return engine.Interrupted(intr)
//
// Hosts inspecting the result use the standard errdefs / errors.As
// idiom:
//
//	if errdefs.IsInterrupted(err) {
//	    var ie engine.InterruptedError
//	    if errors.As(err, &ie) {
//	        switch ie.Cause {
//	        case engine.CauseUserInput: ...
//	        }
//	    }
//	}
//
// A zero-value Interrupt still produces a well-formed error so
// callers don't need to special-case CauseUnknown.
func Interrupted(intr Interrupt) error {
	return interruptedErr{Interrupt: intr}
}

// InterruptedError is the concrete error type returned by [Interrupted].
// It is exported so hosts can use [errors.As] to destructure it; the
// preferred way to produce one is [Interrupted], not direct
// construction.
//
// Implements [errdefs] interrupted-marker so [errdefs.IsInterrupted]
// returns true on any error wrapping (or equal to) one of these.
type InterruptedError = interruptedErr

// interruptedErr is unexported because hosts should construct via
// Interrupted(...) and inspect via errors.As (using the exported alias
// InterruptedError). Keeping the underlying type unexported prevents
// foreign packages from synthesising one without the constructor.
type interruptedErr struct {
	Interrupt
}

// Error formats the cause and detail into a human-readable message.
func (e interruptedErr) Error() string {
	switch {
	case e.Cause == CauseUnknown && e.Detail == "":
		return "engine: interrupted"
	case e.Cause == CauseUnknown:
		return "engine: interrupted: " + e.Detail
	case e.Detail == "":
		return "engine: interrupted (" + string(e.Cause) + ")"
	default:
		return "engine: interrupted (" + string(e.Cause) + "): " + e.Detail
	}
}

// Interrupted is the errdefs marker method, NOT a name accessor. It
// makes errdefs.IsInterrupted(err) report true for any error wrapping
// this type.
func (interruptedErr) Interrupted() {}

// Compile-time assertion that interruptedErr satisfies the errdefs
// interrupted marker (interface{ Interrupted() }). errdefs.IsInterrupted
// uses errors.As against this exact shape, so this assertion guarantees
// classification works.
var _ interface{ Interrupted() } = interruptedErr{}

// MergeInterrupts fans-in N independent interrupt channels into a
// single output channel — the natural shape sandbox / pod hosts need
// to combine "user cancel", "SIGTERM", "budget exceeded", "graceful
// stop", … into the one channel an Engine reads from
// [Host.Interrupts].
//
// Behaviour:
//
//   - The returned channel is closed once ctx is cancelled OR every
//     non-nil source channel has been closed. Engines reading from it
//     therefore see EOF when "everyone is done", matching how a single
//     cooperative-interrupt channel behaves today.
//   - Nil source channels are skipped silently — matches the documented
//     "nil means never fires" semantic in [Interrupter.Interrupts] and
//     keeps callers from having to filter their slice.
//   - When zero non-nil sources are supplied the returned channel is
//     a never-fires channel that is closed when ctx is cancelled. This
//     keeps the helper total — engines selecting on it stay correct
//     even in the trivial case.
//   - Order of forwarded interrupts is the natural runtime arrival
//     order across the source channels. No de-duplication: a host that
//     fires "shutdown" through two distinct sources will surface both.
//
// Forwarding goroutines exit promptly when ctx is cancelled, so the
// helper is safe to use in long-lived hosts that get re-created across
// reloads.
func MergeInterrupts(ctx context.Context, sources ...<-chan Interrupt) <-chan Interrupt {
	out := make(chan Interrupt)

	// Filter nil sources up front so the WaitGroup count matches the
	// number of goroutines actually launched. With ctx cancellation we
	// also need a separate sentinel to wake a parked send when ctx
	// fires before any source produces; a single fan-out goroutine
	// owns that responsibility.
	var live []<-chan Interrupt
	for _, ch := range sources {
		if ch != nil {
			live = append(live, ch)
		}
	}

	var wg sync.WaitGroup
	wg.Add(len(live))
	for _, ch := range live {
		go func(in <-chan Interrupt) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case intr, ok := <-in:
					if !ok {
						return
					}
					select {
					case <-ctx.Done():
						return
					case out <- intr:
					}
				}
			}
		}(ch)
	}

	// Closer: every forwarder exits on ctx.Done OR source close, then
	// wg.Wait unblocks and we close out exactly once with no pending
	// senders. The zero-source case still works: wg starts at 0 and
	// out closes as soon as ctx is cancelled (because the goroutine
	// blocks on wg.Wait, which returns immediately, so we then need a
	// ctx select to keep the channel alive until cancellation). Handle
	// that case explicitly so a 0-source merge doesn't return an
	// already-closed channel.
	go func() {
		if len(live) == 0 {
			<-ctx.Done()
			close(out)
			return
		}
		wg.Wait()
		close(out)
	}()

	return out
}

// ---------- UserPrompt ----------

// UserPrompt describes what the engine is asking the host to relay to
// the end user. It deliberately stays one level below "chat message":
//
//   - Parts carries the multi-modal payload (text, image, audio, file,
//     structured data) using inference.Part — the same building block
//     that inference.Message content uses, minus the chat-specific Role.
//   - Schema is an optional structured-input hint (JSON-schema-shaped
//     bytes) for cases where the host wants to render a form or
//     validate the response.
//   - Source identifies the engine step that produced the prompt;
//     useful for trace correlation and resume.
//   - Metadata is free-form host-passed-through metadata.
//
// Why []inference.Part rather than inference.Message: a Message also
// carries Role (system/user/assistant/tool), which is a chat-layer
// concept the engine has no business naming. Parts give us full
// multi-modality (image, audio, file, data) without tying the engine
// to chat semantics — the agent layer wraps Parts back into a Message
// with the right Role on its way out, and unwraps user-supplied Parts
// on the way in.
type UserPrompt struct {
	Parts    []inference.Part
	Schema   []byte
	Source   string
	Metadata map[string]string
}

// UserReply is what the host returns to satisfy a UserPrompt. Like
// UserPrompt it uses []inference.Part so the response can carry any
// modality the host's user interface produced — typed text, an
// uploaded image, recorded audio, a file attachment, structured form
// data, …
type UserReply struct {
	Parts    []inference.Part
	Metadata map[string]string
}

// ---------- Host context transport ----------

// hostCtxKey is the unexported context key under which engines
// stash the [Host] for downstream consumers (built-in tools that
// need to call host.AskUser, custom plugins that emit envelopes
// via host.Publish, …). Using an unexported key prevents
// collision with caller-supplied values and makes the API the
// only legal way in.
type hostCtxKey struct{}

// ContextWithHost returns a derived context carrying h. Engines that
// dispatch to extension points which were not designed to receive
// the Host directly (sdk/tool's Tool.Execute signature, custom
// plugin callbacks, …) call ContextWithHost before invoking those
// extensions so the extension can recover the Host via
// [HostFromContext].
//
// The intended consumer pattern:
//
//	// engine side: just before invoking the tool registry
//	ctx = agent.ContextWithHost(ctx, host)
//	results := reg.ExecuteAll(ctx, calls)
//
//	// tool side
//	host, ok := agent.HostFromContext(ctx)
//	if ok {
//	    reply, err := host.AskUser(ctx, prompt)
//	}
//
// nil h is allowed and a no-op (returns ctx unchanged) so callers
// can plumb a possibly-nil host without conditional branches.
//
// Engines MUST NOT use the host stashed here as a substitute for
// the host argument they receive in [Engine.Execute] — the
// argument is the contract; the context-carried copy is purely a
// transport for downstream extensions that lack a Host parameter.
func ContextWithHost(ctx context.Context, h Host) context.Context {
	if isNilInterface(h) {
		return ctx
	}
	return context.WithValue(ctx, hostCtxKey{}, h)
}

// HostFromContext returns the Host attached to ctx by a previous
// [ContextWithHost] call, plus an "ok" flag. The ok=false branch means
// either no engine wired the host into ctx (the extension is
// running outside an engine) or the caller passed a nil Host.
//
// Extensions that require host capabilities should treat ok=false
// as a usage error and surface it via the extension's own
// error contract (e.g. ask_user surfaces errdefs.NotAvailable so
// LLMs see "I cannot prompt the user" instead of crashing).
func HostFromContext(ctx context.Context) (Host, bool) {
	if ctx == nil {
		return nil, false
	}
	h, ok := ctx.Value(hostCtxKey{}).(Host)
	if !ok || isNilInterface(h) {
		return nil, false
	}
	return h, true
}
