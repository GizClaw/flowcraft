package route

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

type Selectors struct {
	Generate                     GenerateSelector
	GenerateFallback             GenerateFallbackPolicy
	Embed                        EmbedSelector
	EmbedFallback                EmbedFallbackPolicy
	Transcription                TranscriptionSelector
	TranscriptionFallback        TranscriptionFallbackPolicy
	TranscriptionSession         TranscriptionSessionSelector
	TranscriptionSessionFallback TranscriptionSessionFallbackPolicy
	Realtime                     RealtimeSelector
	RealtimeFallback             RealtimeFallbackPolicy
}

// Decision is the selector output before inference execution. Proposed records
// the selector's initial choice; Selected records any policy-adjusted target.
type Decision struct {
	Operation inference.Operation `json:"operation"`
	Tier      Tier                `json:"tier"`
	Proposed  inference.ModelRef  `json:"proposed"`
	Selected  inference.ModelRef  `json:"selected"`
	Reason    string              `json:"reason,omitempty"`
}

func (d Decision) ValidateFor(operation inference.Operation) error {
	if err := operation.Validate(); err != nil {
		return err
	}
	if d.Operation != operation {
		return fmt.Errorf(
			"route decision operation %q does not match %q",
			d.Operation,
			operation,
		)
	}
	if err := d.Tier.Validate(); err != nil {
		return err
	}
	if err := d.Proposed.Validate(); err != nil {
		return fmt.Errorf("proposed model: %w", err)
	}
	if err := d.Selected.Validate(); err != nil {
		return fmt.Errorf("selected model: %w", err)
	}
	if d.Proposed != d.Selected && d.Reason == "" {
		return fmt.Errorf("changed route target requires a reason")
	}
	return nil
}

type FallbackHop struct {
	From   inference.ModelRef `json:"from"`
	To     inference.ModelRef `json:"to"`
	Reason string             `json:"reason"`
}

type AttemptPhase string

const (
	// AttemptPhasePreflight covers the Explain pass before execution. Only
	// Generate routes preflight: for every other operation the compiler runs
	// as the first pipeline stage, so a local rejection already surfaces at
	// the execute/open phase with a fallback-eligible kind.
	AttemptPhasePreflight AttemptPhase = "preflight"
	// AttemptPhaseExecute covers unary execution (Generate, Embed,
	// Transcribe).
	AttemptPhaseExecute AttemptPhase = "execute"
	// AttemptPhaseOpen covers stream and session opening (GenerateStream,
	// OpenTranscription, OpenRealtime). Once an attempt opens, fallback is
	// over: subsequent failures belong to the caller's stream or session.
	AttemptPhaseOpen AttemptPhase = "open"
)

type AttemptTrigger string

const (
	AttemptTriggerSelection AttemptTrigger = "selection"
	AttemptTriggerRetry     AttemptTrigger = "retry"
	AttemptTriggerFallback  AttemptTrigger = "fallback"
)

type AttemptOutcome string

const (
	AttemptOutcomeSucceeded AttemptOutcome = "succeeded"
	AttemptOutcomeFailed    AttemptOutcome = "failed"
	AttemptOutcomeOpened    AttemptOutcome = "opened"
	AttemptOutcomeSkipped   AttemptOutcome = "skipped"
)

// Attempt records only observable route facts. In particular, an opened
// stream is not reported as completed because its events belong to the caller.
type Attempt struct {
	Target           inference.ModelRef  `json:"target"`
	Phase            AttemptPhase        `json:"phase"`
	Trigger          AttemptTrigger      `json:"trigger"`
	Outcome          AttemptOutcome      `json:"outcome"`
	ErrorKind        inference.ErrorKind `json:"error_kind,omitempty"`
	ObservableOutput bool                `json:"observable_output"`
	// Number is the 1-based attempt number within one target. The initial
	// attempt is 1; retries are 2+.
	Number int `json:"attempt,omitempty"`
	// BackoffMillis is the delay slept before this attempt. Zero for the
	// first attempt.
	BackoffMillis int64 `json:"backoff_ms,omitempty"`
	// Circuit is the circuit state before this attempt ("open", "half_open",
	// or empty when the breaker is disabled).
	Circuit string `json:"circuit,omitempty"`
	// CircuitTransition records a circuit state change caused by this
	// attempt ("open" or "closed"), empty when the state did not change.
	CircuitTransition string `json:"circuit_transition,omitempty"`
	// WireAttempts records the provider-reported HTTP sends inside this
	// logical attempt when the provider propagates the count.
	WireAttempts int `json:"wire_attempts,omitempty"`
	// Transient marks a provider failure classified as retryable/transient.
	// It feeds fallback-on-retry-exhausted decisions.
	Transient bool `json:"transient,omitempty"`
}

// Trace separates route selection from compiler field dispositions. Executed
// records the exact selected model and credential profile after response
// metadata confirms the public model identity. A returned Trace is an immutable
// snapshot: stream and session activity after a successful open never mutates it.
type Trace struct {
	Decision  Decision           `json:"decision"`
	Executed  inference.ModelRef `json:"executed"`
	Fallbacks []FallbackHop      `json:"fallbacks,omitempty"`
	Attempts  []Attempt          `json:"attempts,omitempty"`
}

// Clone returns an owned copy safe to share beyond the call that produced the
// trace.
func (t Trace) Clone() Trace {
	t.Fallbacks = append([]FallbackHop(nil), t.Fallbacks...)
	t.Attempts = append([]Attempt(nil), t.Attempts...)
	return t
}

// Router composes operation-specific selectors above an exact-target inference
// Runtime. Callers that already know the model continue to use Runtime directly.
type Router struct {
	runtime   *inference.Runtime
	selectors Selectors
	retry     RetryPolicies
	breaker   *circuitBreaker
	sleeper   func(context.Context, time.Duration) error
}

// New requires at least one operation selector. A fallback policy without its
// operation selector is a misconfiguration: the policy would never run, so
// New rejects it instead of silently ignoring it.
func New(
	runtime *inference.Runtime,
	selectors Selectors,
	options ...Option,
) (*Router, error) {
	if runtime == nil {
		return nil, errdefs.Validationf("inference runtime is required")
	}
	if isNilInterface(selectors.Generate) &&
		isNilInterface(selectors.Embed) &&
		isNilInterface(selectors.Transcription) &&
		isNilInterface(selectors.TranscriptionSession) &&
		isNilInterface(selectors.Realtime) {
		return nil, errdefs.Validationf("at least one route selector is required")
	}
	orphans := []struct {
		operation inference.Operation
		selector  any
		fallback  any
	}{
		{inference.OperationGenerate, selectors.Generate, selectors.GenerateFallback},
		{inference.OperationEmbed, selectors.Embed, selectors.EmbedFallback},
		{inference.OperationTranscription, selectors.Transcription, selectors.TranscriptionFallback},
		{inference.OperationTranscription, selectors.TranscriptionSession, selectors.TranscriptionSessionFallback},
		{inference.OperationRealtime, selectors.Realtime, selectors.RealtimeFallback},
	}
	for _, orphan := range orphans {
		if !isNilInterface(orphan.fallback) && isNilInterface(orphan.selector) {
			return nil, errdefs.Validationf(
				"%s fallback policy requires a %s selector",
				orphan.operation,
				orphan.operation,
			)
		}
	}
	var configured routerOptions
	configured.clock = time.Now
	configured.sleeper = defaultSleeper
	for index, option := range options {
		if option == nil {
			return nil, errdefs.Validationf("router option %d is nil", index)
		}
		if err := option(&configured); err != nil {
			return nil, errdefs.Validationf("router option %d: %v", index, err)
		}
	}
	if err := validateRetryPolicies(configured.retry, selectors); err != nil {
		return nil, err
	}
	var breaker *circuitBreaker
	if configured.breaker != nil {
		breaker = newCircuitBreaker(*configured.breaker, configured.clock)
	}
	return &Router{
		runtime:   runtime,
		selectors: selectors,
		retry:     configured.retry,
		breaker:   breaker,
		sleeper:   configured.sleeper,
	}, nil
}

func validateRetryPolicies(
	policies RetryPolicies,
	selectors Selectors,
) error {
	entries := []struct {
		operation inference.Operation
		policy    *RetryPolicy
		selector  any
	}{
		{inference.OperationGenerate, policies.Generate, selectors.Generate},
		{inference.OperationEmbed, policies.Embed, selectors.Embed},
		{inference.OperationTranscription, policies.Transcription, selectors.Transcription},
		{inference.OperationTranscription, policies.TranscriptionSession, selectors.TranscriptionSession},
		{inference.OperationRealtime, policies.Realtime, selectors.Realtime},
	}
	for _, entry := range entries {
		if entry.policy == nil {
			continue
		}
		if isNilInterface(entry.selector) {
			return errdefs.Validationf(
				"%s retry policy requires a %s selector",
				entry.operation,
				entry.operation,
			)
		}
		if err := entry.policy.validate(); err != nil {
			return errdefs.Validationf("%s retry policy: %v", entry.operation, err)
		}
	}
	return nil
}

// ResetCircuitBreaker clears all per-target circuit state. In-flight calls
// finish against the breaker they already started; new calls start closed.
func (r *Router) ResetCircuitBreaker() {
	if r.breaker != nil {
		r.breaker.reset()
	}
}

// maxFallbackTargets bounds total targets tried for one request, counting the
// initially selected target.
const maxFallbackTargets = 8

// executeWithFallback runs one unary operation (Generate, Embed, Transcribe)
// across fallback targets with per-target retry/backoff and the circuit
// breaker. snapshot must already be an owned clone; selectors and fallback
// policies receive their own clones so they cannot mutate the request being
// executed. preflight may be nil: without it the compiler runs inside execute,
// and a local rejection still surfaces with a fallback-eligible kind before
// any provider I/O.
func executeWithFallback[Request any, Response any](
	r *Router,
	ctx context.Context,
	operation inference.Operation,
	snapshot Request,
	clone func(Request) Request,
	validate func(Request) error,
	selector any,
	selectRequest func(context.Context, Request) (Decision, error),
	fallbackNext func(context.Context, Request, Attempt) (inference.ModelRef, bool, error),
	preflight func(context.Context, inference.ModelRef, Request) error,
	execute func(context.Context, inference.ModelRef, Request) (Response, inference.Metadata, error),
) (Response, Trace, error) {
	var zero Response
	decision, err := selectTarget(
		ctx, r.runtime, operation, snapshot, clone, validate, selector, selectRequest,
	)
	if err != nil {
		return zero, Trace{}, err
	}
	trace := Trace{Decision: decision}
	target := decision.Selected
	trigger := AttemptTriggerSelection
	seen := map[inference.ModelRef]struct{}{target: {}}
	policy := r.retry.policyFor(operation).effective()
	var lastErr error
	totalAttempts := 0
	for {
		breakerAllowed := false
		var key circuitKey
		if r.breaker != nil {
			key = circuitKey{operation: operation, model: target}
			gate := r.breaker.begin(ctx, key)
			breakerAllowed = gate.allowed
			if !breakerAllowed {
				trace.Attempts = append(trace.Attempts, Attempt{
					Target: target, Phase: AttemptPhaseExecute, Trigger: trigger,
					Outcome: AttemptOutcomeSkipped, Circuit: string(gate.state),
				})
				next, ok, fallbackErr := nextFallbackTarget(r,
					ctx, operation, snapshot, clone,
					Attempt{Target: target, Phase: AttemptPhaseExecute, Trigger: trigger, Outcome: AttemptOutcomeSkipped},
					&trace, seen, fallbackNext, true, false, AttemptPhaseExecute,
				)
				if fallbackErr != nil {
					return zero, trace, fallbackErr
				}
				if !ok {
					if lastErr != nil {
						return zero, trace, lastErr
					}
					return zero, trace, NewError(
						CircuitOpen,
						operation,
						errors.New("all route targets are circuit-open"),
					)
				}
				target, trigger = next, AttemptTriggerFallback
				continue
			}
		}

		attemptNumber := 0
		var backoffMillis int64
		for {
			if policy.MaxTotalAttempts > 0 &&
				totalAttempts >= policy.MaxTotalAttempts {
				if lastErr != nil {
					return zero, trace, lastErr
				}
				return zero, trace, errors.New("retry total attempt budget exhausted")
			}
			attemptNumber++
			totalAttempts++
			attemptTrigger := trigger
			if attemptNumber > 1 {
				attemptTrigger = AttemptTriggerRetry
			}
			if attemptNumber > 1 {
				delay := retryDelay(policy.Backoff, attemptNumber-1, lastErr)
				if err := r.sleeper(ctx, delay); err != nil {
					return zero, trace, err
				}
				backoffMillis = delay.Milliseconds()
			}
			if preflight != nil {
				if err := preflight(ctx, target, snapshot); err != nil {
					attempt := failedAttempt(target, AttemptPhasePreflight, attemptTrigger, err)
					attempt.Number = attemptNumber
					attempt.BackoffMillis = backoffMillis
					if r.breaker != nil {
						attempt.CircuitTransition = r.breaker.finish(
							ctx, key, err, false,
						)
					}
					trace.Attempts = append(trace.Attempts, attempt)
					if policy.MaxTotalAttempts > 0 &&
						totalAttempts >= policy.MaxTotalAttempts {
						return zero, trace, err
					}
					if fallbackEligible(attempt) {
						next, ok, fallbackErr := nextFallbackTarget(r,
							ctx, operation, snapshot, clone,
							attempt, &trace, seen, fallbackNext, false, false, AttemptPhaseExecute,
						)
						if fallbackErr != nil {
							return zero, trace, fallbackErr
						}
						if !ok {
							return zero, trace, err
						}
						target, trigger = next, AttemptTriggerFallback
						break
					}
					decision := retryDecision(
						operation, AttemptPhasePreflight, attemptTrigger, err, attemptNumber,
					)
					if retryEligible(ctx, &policy, decision) {
						lastErr = err
						continue
					}
					allowTransient := policy.FallbackOnRetryExhausted && attempt.Transient
					next, ok, fallbackErr := nextFallbackTarget(r,
						ctx, operation, snapshot, clone,
						attempt, &trace, seen, fallbackNext, false, allowTransient, AttemptPhaseExecute,
					)
					if fallbackErr != nil {
						return zero, trace, fallbackErr
					}
					if !ok {
						return zero, trace, err
					}
					target, trigger = next, AttemptTriggerFallback
					break
				}
				trace.Attempts = append(trace.Attempts, Attempt{
					Target: target, Phase: AttemptPhasePreflight, Trigger: trigger,
					Outcome: AttemptOutcomeSucceeded, Number: attemptNumber,
				})
			}
			response, metadata, err := execute(ctx, target, snapshot)
			if err != nil {
				attempt := failedAttempt(target, AttemptPhaseExecute, attemptTrigger, err)
				attempt.Number = attemptNumber
				attempt.BackoffMillis = backoffMillis
				if r.breaker != nil {
					attempt.CircuitTransition = r.breaker.finish(
						ctx, key, err, attempt.Transient,
					)
				}
				trace.Attempts = append(trace.Attempts, attempt)
				if policy.MaxTotalAttempts > 0 &&
					totalAttempts >= policy.MaxTotalAttempts {
					return zero, trace, err
				}
				decision := retryDecision(
					operation, AttemptPhaseExecute, attemptTrigger, err, attemptNumber,
				)
				if retryEligible(ctx, &policy, decision) {
					lastErr = err
					continue
				}
				allowTransient := policy.FallbackOnRetryExhausted && attempt.Transient
				next, ok, fallbackErr := nextFallbackTarget(r,
					ctx, operation, snapshot, clone,
					attempt, &trace, seen, fallbackNext, false, allowTransient, AttemptPhaseExecute,
				)
				if fallbackErr != nil {
					return zero, trace, fallbackErr
				}
				if !ok {
					return zero, trace, err
				}
				target, trigger = next, AttemptTriggerFallback
				break
			}
			transition := ""
			if r.breaker != nil {
				transition = r.breaker.finish(ctx, key, nil, false)
			}
			trace.Attempts = append(trace.Attempts, Attempt{
				Target: target, Phase: AttemptPhaseExecute, Trigger: attemptTrigger,
				Outcome: AttemptOutcomeSucceeded, Number: attemptNumber,
				CircuitTransition: transition,
			})
			trace.Executed = target
			if metadata.Operation != operation || metadata.Model != target.ID {
				return zero, trace, NewError(
					SelectorContractViolation,
					operation,
					errors.New("inference response does not match selected route"),
				)
			}
			return response, trace, nil
		}
	}
}

// openSessionWithFallback opens a stream or session across fallback targets
// with per-target retry/backoff and the circuit breaker. Fallback and retry
// only exist before open: an opened session is owned by the caller and never
// migrates. preflight is optional and only used by GenerateStream; without it
// opening already compiles locally before any provider I/O, so a compiler
// rejection surfaces with a fallback-eligible kind.
func openSessionWithFallback[Request any, Session any](
	r *Router,
	ctx context.Context,
	operation inference.Operation,
	snapshot Request,
	clone func(Request) Request,
	validate func(Request) error,
	selector any,
	selectRequest func(context.Context, Request) (Decision, error),
	fallbackNext func(context.Context, Request, Attempt) (inference.ModelRef, bool, error),
	preflight func(context.Context, inference.ModelRef, Request) error,
	open func(context.Context, inference.ModelRef, Request) (Session, error),
) (Session, Trace, error) {
	var zero Session
	decision, err := selectTarget(
		ctx, r.runtime, operation, snapshot, clone, validate, selector, selectRequest,
	)
	if err != nil {
		return zero, Trace{}, err
	}
	trace := Trace{Decision: decision}
	target := decision.Selected
	trigger := AttemptTriggerSelection
	seen := map[inference.ModelRef]struct{}{target: {}}
	policy := r.retry.sessionPolicyFor(operation).effective()
	skipPhase := AttemptPhaseOpen
	if preflight != nil {
		skipPhase = AttemptPhasePreflight
	}
	var lastErr error
	totalAttempts := 0
	for {
		breakerAllowed := false
		var key circuitKey
		if r.breaker != nil {
			key = circuitKey{operation: operation, model: target}
			gate := r.breaker.begin(ctx, key)
			breakerAllowed = gate.allowed
			if !breakerAllowed {
				trace.Attempts = append(trace.Attempts, Attempt{
					Target: target, Phase: skipPhase, Trigger: trigger,
					Outcome: AttemptOutcomeSkipped, Circuit: string(gate.state),
				})
				next, ok, fallbackErr := nextFallbackTarget(r,
					ctx, operation, snapshot, clone,
					Attempt{Target: target, Phase: skipPhase, Trigger: trigger, Outcome: AttemptOutcomeSkipped},
					&trace, seen, fallbackNext, true, false, skipPhase,
				)
				if fallbackErr != nil {
					return zero, trace, fallbackErr
				}
				if !ok {
					if lastErr != nil {
						return zero, trace, lastErr
					}
					return zero, trace, NewError(
						CircuitOpen,
						operation,
						errors.New("all route targets are circuit-open"),
					)
				}
				target, trigger = next, AttemptTriggerFallback
				continue
			}
		}

		attemptNumber := 0
		var backoffMillis int64
		for {
			if policy.MaxTotalAttempts > 0 &&
				totalAttempts >= policy.MaxTotalAttempts {
				if lastErr != nil {
					return zero, trace, lastErr
				}
				return zero, trace, errors.New("retry total attempt budget exhausted")
			}
			attemptNumber++
			totalAttempts++
			attemptTrigger := trigger
			if attemptNumber > 1 {
				attemptTrigger = AttemptTriggerRetry
			}
			if attemptNumber > 1 {
				delay := retryDelay(policy.Backoff, attemptNumber-1, lastErr)
				if err := r.sleeper(ctx, delay); err != nil {
					return zero, trace, err
				}
				backoffMillis = delay.Milliseconds()
			}
			if preflight != nil {
				if err := preflight(ctx, target, snapshot); err != nil {
					attempt := failedAttempt(target, AttemptPhasePreflight, attemptTrigger, err)
					attempt.Number = attemptNumber
					attempt.BackoffMillis = backoffMillis
					if r.breaker != nil {
						attempt.CircuitTransition = r.breaker.finish(
							ctx, key, err, false,
						)
					}
					trace.Attempts = append(trace.Attempts, attempt)
					if policy.MaxTotalAttempts > 0 &&
						totalAttempts >= policy.MaxTotalAttempts {
						return zero, trace, err
					}
					if fallbackEligible(attempt) {
						next, ok, fallbackErr := nextFallbackTarget(r,
							ctx, operation, snapshot, clone,
							attempt, &trace, seen, fallbackNext, false, false, skipPhase,
						)
						if fallbackErr != nil {
							return zero, trace, fallbackErr
						}
						if !ok {
							return zero, trace, err
						}
						target, trigger = next, AttemptTriggerFallback
						break
					}
					if retryEligible(ctx, &policy, retryDecision(
						operation, AttemptPhasePreflight, attemptTrigger, err, attemptNumber,
					)) {
						lastErr = err
						continue
					}
					allowTransient := policy.FallbackOnRetryExhausted && attempt.Transient
					next, ok, fallbackErr := nextFallbackTarget(r,
						ctx, operation, snapshot, clone,
						attempt, &trace, seen, fallbackNext, false, allowTransient, skipPhase,
					)
					if fallbackErr != nil {
						return zero, trace, fallbackErr
					}
					if !ok {
						return zero, trace, err
					}
					target, trigger = next, AttemptTriggerFallback
					break
				}
				trace.Attempts = append(trace.Attempts, Attempt{
					Target: target, Phase: AttemptPhasePreflight, Trigger: trigger,
					Outcome: AttemptOutcomeSucceeded, Number: attemptNumber,
				})
			}
			session, err := open(ctx, target, snapshot)
			if err != nil {
				attempt := failedAttempt(target, AttemptPhaseOpen, attemptTrigger, err)
				attempt.Number = attemptNumber
				attempt.BackoffMillis = backoffMillis
				if r.breaker != nil {
					attempt.CircuitTransition = r.breaker.finish(
						ctx, key, err, attempt.Transient,
					)
				}
				trace.Attempts = append(trace.Attempts, attempt)
				if policy.MaxTotalAttempts > 0 &&
					totalAttempts >= policy.MaxTotalAttempts {
					return zero, trace, err
				}
				if retryEligible(ctx, &policy, retryDecision(
					operation, AttemptPhaseOpen, attemptTrigger, err, attemptNumber,
				)) {
					lastErr = err
					continue
				}
				allowTransient := policy.FallbackOnRetryExhausted && attempt.Transient
				next, ok, fallbackErr := nextFallbackTarget(r,
					ctx, operation, snapshot, clone,
					attempt, &trace, seen, fallbackNext, false, allowTransient, skipPhase,
				)
				if fallbackErr != nil {
					return zero, trace, fallbackErr
				}
				if !ok {
					return zero, trace, err
				}
				target, trigger = next, AttemptTriggerFallback
				break
			}
			transition := ""
			if r.breaker != nil {
				transition = r.breaker.finish(ctx, key, nil, false)
			}
			trace.Attempts = append(trace.Attempts, Attempt{
				Target: target, Phase: AttemptPhaseOpen, Trigger: attemptTrigger,
				Outcome: AttemptOutcomeOpened, Number: attemptNumber,
				CircuitTransition: transition,
			})
			trace.Executed = target
			return session, trace, nil
		}
	}
}

// nextFallbackTarget asks the operation's fallback policy for another target
// and enforces the shared contract: transport-safe eligibility, bounded target
// count, valid and previously unattempted targets, runtime-confirmed operation
// support, and circuit-open skips. skipEligibility bypasses the transport-safe
// gate for circuit-open skips; allowTransient permits retry-exhausted transient
// provider failures when the operation policy opts in.
func nextFallbackTarget[Request any](
	r *Router,
	ctx context.Context,
	operation inference.Operation,
	snapshot Request,
	clone func(Request) Request,
	attempt Attempt,
	trace *Trace,
	seen map[inference.ModelRef]struct{},
	fallbackNext func(context.Context, Request, Attempt) (inference.ModelRef, bool, error),
	skipEligibility bool,
	allowTransient bool,
	skipPhase AttemptPhase,
) (inference.ModelRef, bool, error) {
	if fallbackNext == nil {
		return inference.ModelRef{}, false, nil
	}
	if !skipEligibility {
		eligible := fallbackEligible(attempt)
		if !eligible && (!allowTransient || !attempt.Transient) {
			return inference.ModelRef{}, false, nil
		}
	}
	next, ok, err := fallbackNext(ctx, clone(snapshot), attempt)
	if err != nil {
		var routeErr *Error
		if errors.As(err, &routeErr) {
			return inference.ModelRef{}, false, err
		}
		return inference.ModelRef{}, false, NewError(FallbackFailed, operation, err)
	}
	if !ok {
		if next != (inference.ModelRef{}) {
			return inference.ModelRef{}, false, NewError(
				FallbackContractViolation,
				operation,
				errors.New("fallback stop returned a target"),
			)
		}
		return inference.ModelRef{}, false, nil
	}
	for {
		if len(seen) >= maxFallbackTargets {
			return inference.ModelRef{}, false, NewError(
				FallbackLimitExceeded,
				operation,
				fmt.Errorf("%s fallback exceeds %d targets", operation, maxFallbackTargets),
			)
		}
		if err := next.Validate(); err != nil {
			return inference.ModelRef{}, false, NewError(
				FallbackContractViolation,
				operation,
				fmt.Errorf("invalid fallback target: %w", err),
			)
		}
		if _, duplicate := seen[next]; duplicate {
			return inference.ModelRef{}, false, NewError(
				FallbackContractViolation,
				operation,
				errors.New("fallback returned a previously attempted target"),
			)
		}
		descriptor, err := r.runtime.InspectModel(next)
		if err != nil {
			return inference.ModelRef{}, false, NewError(
				FallbackContractViolation,
				operation,
				err,
			)
		}
		if !supportsOperation(descriptor, operation) {
			return inference.ModelRef{}, false, NewError(
				FallbackContractViolation,
				operation,
				errors.New("fallback returned a model without the operation"),
			)
		}
		if descriptor.Lifecycle.Status == inference.ModelStatusRetired {
			return inference.ModelRef{}, false, NewError(
				FallbackContractViolation,
				operation,
				errors.New("fallback returned a retired model"),
			)
		}
		if r.breaker != nil && r.breaker.isOpen(circuitKey{
			operation: operation, model: next,
		}) {
			seen[next] = struct{}{}
			trace.Attempts = append(trace.Attempts, Attempt{
				Target: next, Phase: skipPhase, Trigger: AttemptTriggerFallback,
				Outcome: AttemptOutcomeSkipped, Circuit: "open",
			})
			if len(seen) >= maxFallbackTargets {
				return inference.ModelRef{}, false, NewError(
					FallbackLimitExceeded,
					operation,
					fmt.Errorf("%s fallback exceeds %d targets", operation, maxFallbackTargets),
				)
			}
			next, ok, err = fallbackNext(ctx, clone(snapshot), Attempt{
				Target: next, Phase: skipPhase, Trigger: AttemptTriggerFallback,
				Outcome: AttemptOutcomeSkipped, Circuit: "open",
			})
			if err != nil {
				var routeErr *Error
				if errors.As(err, &routeErr) {
					return inference.ModelRef{}, false, err
				}
				return inference.ModelRef{}, false, NewError(FallbackFailed, operation, err)
			}
			if !ok {
				if next != (inference.ModelRef{}) {
					return inference.ModelRef{}, false, NewError(
						FallbackContractViolation,
						operation,
						errors.New("fallback stop returned a target"),
					)
				}
				return inference.ModelRef{}, false, nil
			}
			continue
		}
		seen[next] = struct{}{}
		trace.Fallbacks = append(trace.Fallbacks, FallbackHop{
			From: attempt.Target, To: next, Reason: string(attempt.ErrorKind),
		})
		return next, true, nil
	}
}

// failedAttempt records a failed attempt with the inference error kind that
// drives fallback eligibility. Non-inference errors leave ErrorKind empty,
// which is never eligible.
func failedAttempt(
	target inference.ModelRef,
	phase AttemptPhase,
	trigger AttemptTrigger,
	err error,
) Attempt {
	attempt := Attempt{
		Target: target, Phase: phase, Trigger: trigger,
		Outcome: AttemptOutcomeFailed,
	}
	var inferenceErr *inference.Error
	if errors.As(err, &inferenceErr) {
		attempt.ErrorKind = inferenceErr.Kind
		attempt.WireAttempts = inferenceErr.WireAttempts
	}
	attempt.Transient = transientProviderFailure(err)
	return attempt
}

func retryDecision(
	operation inference.Operation,
	phase AttemptPhase,
	trigger AttemptTrigger,
	err error,
	attempt int,
) RetryDecision {
	decision := RetryDecision{
		Operation: operation,
		Phase:     phase,
		Trigger:   trigger,
		Err:       err,
		Attempt:   attempt,
	}
	var inferenceErr *inference.Error
	if errors.As(err, &inferenceErr) {
		decision.ErrorKind = inferenceErr.Kind
	}
	return decision
}

// fallbackEligible reports whether a failed attempt is transport-safe to
// retry on another exact target: it must have failed before any observable
// output with a local compiler rejection kind.
func fallbackEligible(attempt Attempt) bool {
	if attempt.Outcome != AttemptOutcomeFailed || attempt.ObservableOutput {
		return false
	}
	return fallbackEligibleKind(attempt.ErrorKind)
}

// selectTarget validates the snapshot, asks the selector for an exact target,
// and confirms the target exists, supports the operation, and is not retired.
// snapshot must already be an owned clone; the selector receives its own clone.
func selectTarget[Request any](
	ctx context.Context,
	runtime *inference.Runtime,
	operation inference.Operation,
	snapshot Request,
	clone func(Request) Request,
	validate func(Request) error,
	selector any,
	selectRequest func(context.Context, Request) (Decision, error),
) (Decision, error) {
	if err := validate(snapshot); err != nil {
		return Decision{}, NewError(InvalidRequest, operation, err)
	}
	if isNilInterface(selector) {
		return Decision{}, NewError(
			SelectorUnavailable,
			operation,
			errors.New("selector is not configured"),
		)
	}
	decision, err := selectRequest(ctx, clone(snapshot))
	if err != nil {
		var routeErr *Error
		if errors.As(err, &routeErr) {
			return Decision{}, err
		}
		return Decision{}, NewError(SelectionFailed, operation, err)
	}
	if err := decision.ValidateFor(operation); err != nil {
		return Decision{}, NewError(SelectorContractViolation, operation, err)
	}
	descriptor, err := runtime.InspectModel(decision.Selected)
	if err != nil {
		return Decision{}, NewError(SelectionFailed, operation, err)
	}
	if descriptor.Lifecycle.Status == inference.ModelStatusRetired {
		return Decision{}, NewError(
			SelectorContractViolation,
			operation,
			errors.New("selector returned a retired model"),
		)
	}
	if !supportsOperation(descriptor, operation) {
		return Decision{}, NewError(
			SelectorContractViolation,
			operation,
			errors.New("selector returned a model without the operation"),
		)
	}
	return decision, nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
