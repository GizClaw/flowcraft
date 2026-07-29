package route

import (
	"context"
	"errors"
	"fmt"
	"reflect"

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
	AttemptTriggerFallback  AttemptTrigger = "fallback"
)

type AttemptOutcome string

const (
	AttemptOutcomeSucceeded AttemptOutcome = "succeeded"
	AttemptOutcomeFailed    AttemptOutcome = "failed"
	AttemptOutcomeOpened    AttemptOutcome = "opened"
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
}

// Trace separates route selection from compiler field dispositions. Executed
// records the exact selected model and credential profile after response
// metadata confirms the public model identity. GenerateStream returns a Trace
// whose opened attempt shares internal state with the stream: read that Trace
// only after a Next call has returned, never concurrently with Next. Successful
// Next calls set the opened attempt's ObservableOutput.
type Trace struct {
	Decision  Decision           `json:"decision"`
	Executed  inference.ModelRef `json:"executed"`
	Fallbacks []FallbackHop      `json:"fallbacks,omitempty"`
	Attempts  []Attempt          `json:"attempts,omitempty"`
}

// Clone returns an owned copy safe to share beyond the call that produced the
// trace. GenerateStream callers must still respect the read-after-Next rule:
// Clone copies the attempt values observed at call time.
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
}

// New requires at least one operation selector. A fallback policy without its
// operation selector is a misconfiguration: the policy would never run, so
// New rejects it instead of silently ignoring it.
func New(runtime *inference.Runtime, selectors Selectors) (*Router, error) {
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
	return &Router{runtime: runtime, selectors: selectors}, nil
}

// maxFallbackTargets bounds total targets tried for one request, counting the
// initially selected target.
const maxFallbackTargets = 8

// executeWithFallback runs one unary operation (Generate, Embed, Transcribe)
// across fallback targets. snapshot must already be an owned clone; selectors
// and fallback policies receive their own clones so they cannot mutate the
// request being executed. preflight may be nil: without it the compiler runs
// inside execute, and a local rejection still surfaces with a fallback-eligible
// kind before any provider I/O.
func executeWithFallback[Request any, Response any](
	ctx context.Context,
	runtime *inference.Runtime,
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
		ctx, runtime, operation, snapshot, clone, validate, selector, selectRequest,
	)
	if err != nil {
		return zero, Trace{}, err
	}
	trace := Trace{Decision: decision}
	target := decision.Selected
	trigger := AttemptTriggerSelection
	seen := map[inference.ModelRef]struct{}{target: {}}
	for {
		if preflight != nil {
			if err := preflight(ctx, target, snapshot); err != nil {
				attempt := failedAttempt(target, AttemptPhasePreflight, trigger, err)
				trace.Attempts = append(trace.Attempts, attempt)
				next, ok, fallbackErr := nextFallbackTarget(
					ctx, runtime, operation, snapshot, clone,
					attempt, &trace, seen, fallbackNext,
				)
				if fallbackErr != nil {
					return zero, trace, fallbackErr
				}
				if !ok {
					return zero, trace, err
				}
				target, trigger = next, AttemptTriggerFallback
				continue
			}
			trace.Attempts = append(trace.Attempts, Attempt{
				Target: target, Phase: AttemptPhasePreflight, Trigger: trigger,
				Outcome: AttemptOutcomeSucceeded,
			})
		}
		response, metadata, err := execute(ctx, target, snapshot)
		if err != nil {
			attempt := failedAttempt(target, AttemptPhaseExecute, trigger, err)
			trace.Attempts = append(trace.Attempts, attempt)
			next, ok, fallbackErr := nextFallbackTarget(
				ctx, runtime, operation, snapshot, clone,
				attempt, &trace, seen, fallbackNext,
			)
			if fallbackErr != nil {
				return zero, trace, fallbackErr
			}
			if !ok {
				return zero, trace, err
			}
			target, trigger = next, AttemptTriggerFallback
			continue
		}
		trace.Attempts = append(trace.Attempts, Attempt{
			Target: target, Phase: AttemptPhaseExecute, Trigger: trigger,
			Outcome: AttemptOutcomeSucceeded,
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

// openSessionWithFallback opens a stream or session across fallback targets
// (OpenTranscription, OpenRealtime). Fallback only exists before open: an
// opened session is owned by the caller and never migrates. There is no
// preflight pass because opening already compiles locally before any provider
// I/O, so a compiler rejection surfaces with a fallback-eligible kind.
func openSessionWithFallback[Request any, Session any](
	ctx context.Context,
	runtime *inference.Runtime,
	operation inference.Operation,
	snapshot Request,
	clone func(Request) Request,
	validate func(Request) error,
	selector any,
	selectRequest func(context.Context, Request) (Decision, error),
	fallbackNext func(context.Context, Request, Attempt) (inference.ModelRef, bool, error),
	open func(context.Context, inference.ModelRef, Request) (Session, error),
) (Session, Trace, error) {
	var zero Session
	decision, err := selectTarget(
		ctx, runtime, operation, snapshot, clone, validate, selector, selectRequest,
	)
	if err != nil {
		return zero, Trace{}, err
	}
	trace := Trace{Decision: decision}
	target := decision.Selected
	trigger := AttemptTriggerSelection
	seen := map[inference.ModelRef]struct{}{target: {}}
	for {
		session, err := open(ctx, target, snapshot)
		if err != nil {
			attempt := failedAttempt(target, AttemptPhaseOpen, trigger, err)
			trace.Attempts = append(trace.Attempts, attempt)
			next, ok, fallbackErr := nextFallbackTarget(
				ctx, runtime, operation, snapshot, clone,
				attempt, &trace, seen, fallbackNext,
			)
			if fallbackErr != nil {
				return zero, trace, fallbackErr
			}
			if !ok {
				return zero, trace, err
			}
			target, trigger = next, AttemptTriggerFallback
			continue
		}
		trace.Attempts = append(trace.Attempts, Attempt{
			Target: target, Phase: AttemptPhaseOpen, Trigger: trigger,
			Outcome: AttemptOutcomeOpened,
		})
		trace.Executed = target
		return session, trace, nil
	}
}

// nextFallbackTarget asks the operation's fallback policy for another target
// and enforces the shared contract: transport-safe eligibility, bounded target
// count, valid and previously unattempted targets, and runtime-confirmed
// operation support. A nil fallbackNext disables fallback for the operation.
func nextFallbackTarget[Request any](
	ctx context.Context,
	runtime *inference.Runtime,
	operation inference.Operation,
	snapshot Request,
	clone func(Request) Request,
	attempt Attempt,
	trace *Trace,
	seen map[inference.ModelRef]struct{},
	fallbackNext func(context.Context, Request, Attempt) (inference.ModelRef, bool, error),
) (inference.ModelRef, bool, error) {
	if fallbackNext == nil || !fallbackEligible(attempt) {
		return inference.ModelRef{}, false, nil
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
	descriptor, err := runtime.InspectModel(next)
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
	seen[next] = struct{}{}
	trace.Fallbacks = append(trace.Fallbacks, FallbackHop{
		From: attempt.Target, To: next, Reason: string(attempt.ErrorKind),
	})
	return next, true, nil
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
	}
	return attempt
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
