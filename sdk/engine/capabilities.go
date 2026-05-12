package engine

// Capabilities describes the optional features an Engine implementation
// declares to its host. The pod / agent layer reads these to:
//
//   - validate a PodSpec at Apply time (e.g. RestartPolicy=Always
//     requires SupportsResume=true, otherwise the spec is rejected
//     before any work starts);
//   - decide which hooks to wire (configure CheckpointStore only when
//     EmitsCheckpoint is true);
//   - refuse to run an engine that needs user prompts in a headless
//     deployment (EmitsUserPrompt=true on a host without an interactive
//     channel becomes a fail-fast).
//
// All fields default to "do not claim the capability" (zero value).
// Engines that do not satisfy the [Describer] interface are treated as
// the zero Capabilities — the most conservative assumption.
type Capabilities struct {
	// SupportsResume reports whether Execute can honour
	// Run.ResumeFrom. Engines that always return errdefs.NotAvailable
	// for a non-nil ResumeFrom MUST leave this false; pods enforcing
	// RestartPolicy=Always need the true case to recover mid-run state
	// without losing partial work.
	SupportsResume bool

	// EmitsUserPrompt reports whether the engine may call
	// Host.AskUser during Execute. Pods deploying in headless / batch
	// mode use this to refuse engines that would block waiting for a
	// reply that nobody can supply.
	EmitsUserPrompt bool

	// EmitsCheckpoint reports whether the engine writes Checkpoints
	// during Execute (independently of SupportsResume — an engine
	// can write checkpoints that only an external tool can replay).
	// Pods use this to decide whether to attach a CheckpointStore.
	EmitsCheckpoint bool

	// RequiredDepNames lists the named dependencies the engine
	// expects in Run.Deps. Names follow the convention enumerated
	// in the [sdk/engine/depname] package — engines reference the
	// constants there rather than open-coding strings so a typo
	// becomes a compile-time error. Hosts populate Dependencies
	// under the same names the engine declares here.
	//
	// Pods, agent.Run pre-flight, and the vessel build path
	// iterate this list and reject the spec / run when a required
	// dep is absent — surfacing wiring mistakes before any
	// engine.Execute call. Empty when the engine has no required
	// deps.
	//
	// This is a *named* declaration deliberately. The underlying
	// Dependencies map is keyed by any so engines cannot
	// meaningfully expose their concrete typed keys to a generic
	// pod controller; the string convention is the only thing
	// dashboards / admin APIs can serialise.
	//
	// [sdk/engine/depname]: https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk/engine/depname
	RequiredDepNames []string
}

// Describer is the optional interface an Engine implementation
// satisfies to advertise its [Capabilities]. Hosts that need to
// gate behaviour on capabilities type-assert on Engine; engines that
// do not implement Describer are treated as having the zero
// Capabilities (no features claimed) — the safe default.
//
// Kept as a separate optional interface (not folded into Engine)
// because most engines need only Execute and the SDK contract
// MUST stay easy to satisfy with a one-method type.
type Describer interface {
	Capabilities() Capabilities
}

// CapabilitiesOf returns the engine's declared capabilities, or the
// zero value when the engine does not implement [Describer]. Hosts
// SHOULD use this helper rather than ad-hoc type assertions so the
// "missing = zero" convention stays uniform.
func CapabilitiesOf(e Engine) Capabilities {
	if d, ok := e.(Describer); ok {
		return d.Capabilities()
	}
	return Capabilities{}
}

// WithCapabilities wraps eng so it advertises caps via the
// [Describer] interface. Use it when the underlying engine cannot
// add a Capabilities() method itself — typically EngineFunc-based
// adapters where the engine value is a function type and methods
// cannot carry extra state.
//
// Example:
//
//	eng := engine.WithCapabilities(
//	    engine.EngineFunc(func(...) (...) { ... }),
//	    engine.Capabilities{
//	        EmitsCheckpoint:  true,
//	        RequiredDepNames: []string{depname.LLMClient},
//	    })
//
// Wrapping is a no-op view: the wrapper forwards every call to the
// underlying engine and also satisfies any optional interface the
// underlying engine satisfies (Resumer / CheckpointSuggester) via
// type-assertion delegation in the per-feature helpers
// (AsResumer / SuggestCheckpoint). The capability claim is the only
// behaviour the wrapper adds.
func WithCapabilities(eng Engine, caps Capabilities) Engine {
	if eng == nil {
		return nil
	}
	return engineWithCaps{Engine: eng, caps: caps}
}

// engineWithCaps is the concrete adapter returned by
// [WithCapabilities]. Exported only via the Engine interface so the
// wrapper is opaque to consumers.
type engineWithCaps struct {
	Engine
	caps Capabilities
}

// Capabilities satisfies [Describer].
func (e engineWithCaps) Capabilities() Capabilities { return e.caps }

// Unwrap exposes the underlying engine so optional interface probes
// (AsResumer, type-assertion to CheckpointSuggester, …) can reach
// past the wrapper. Standard errors.As-style unwrap convention.
func (e engineWithCaps) Unwrap() Engine { return e.Engine }

// CheckpointSuggester is the optional engine-side interface a host
// uses to ask the engine to write a Checkpoint at the next safe
// boundary — typically before a voluntary restart, scale-down, or pod
// reschedule.
//
// Semantics (advisory, not synchronous):
//
//   - The engine SHOULD call its host's Checkpointer at the next
//     point in execution where Checkpoint.Step is well-defined. It is
//     NOT obligated to interrupt itself; SuggestCheckpoint returns
//     immediately with no guarantee that the checkpoint has been
//     written by the time it returns.
//   - The host typically pairs SuggestCheckpoint with a follow-up
//     Interrupt after a grace period: "save what you can, then stop".
//   - Engines that have no notion of a step boundary (purely
//     memory-resident, sub-second runs) MAY treat this as a no-op.
//   - Calling SuggestCheckpoint on an engine that does not implement
//     this interface is impossible by the type system; hosts use
//     [SuggestCheckpoint] (the helper below) which type-asserts and
//     no-ops on engines that do not satisfy the interface.
type CheckpointSuggester interface {
	SuggestCheckpoint() error
}

// SuggestCheckpoint asks the engine for a voluntary checkpoint when
// the engine implements [CheckpointSuggester]; otherwise it is a
// no-op. Returns the engine's error directly so the host can log /
// retry; nil is returned both for "engine does not support
// suggestion" and for "engine accepted the suggestion".
//
// Walks any [WithCapabilities]-style wrappers via Unwrap so a
// suggester wrapped to advertise capabilities still surfaces.
func SuggestCheckpoint(e Engine) error {
	for e != nil {
		if s, ok := e.(CheckpointSuggester); ok {
			return s.SuggestCheckpoint()
		}
		u, ok := e.(interface{ Unwrap() Engine })
		if !ok {
			return nil
		}
		e = u.Unwrap()
	}
	return nil
}
