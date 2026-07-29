package route

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

type GenerateSelector interface {
	SelectGenerate(context.Context, inference.GenerateRequest) (Decision, error)
}

// GenerateFallbackPolicy chooses another exact target after a structured,
// transport-safe failed attempt. Both the request and attempt are snapshots;
// returning ok=false stops fallback.
type GenerateFallbackPolicy interface {
	NextGenerate(
		context.Context,
		inference.GenerateRequest,
		Attempt,
	) (inference.ModelRef, bool, error)
}

type EmbedSelector interface {
	SelectEmbed(context.Context, inference.EmbedRequest) (Decision, error)
}

type TranscriptionSelector interface {
	SelectTranscription(context.Context, inference.TranscriptionRequest) (Decision, error)
}

type TranscriptionSessionSelector interface {
	SelectTranscriptionSession(
		context.Context,
		inference.TranscriptionSessionConfig,
	) (Decision, error)
}

type RealtimeSelector interface {
	SelectRealtime(context.Context, inference.RealtimeConfig) (Decision, error)
}

type Selectors struct {
	Generate             GenerateSelector
	GenerateFallback     GenerateFallbackPolicy
	Embed                EmbedSelector
	Transcription        TranscriptionSelector
	TranscriptionSession TranscriptionSessionSelector
	Realtime             RealtimeSelector
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
	AttemptPhasePreflight  AttemptPhase = "preflight"
	AttemptPhaseExecute    AttemptPhase = "execute"
	AttemptPhaseStreamOpen AttemptPhase = "stream_open"
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
	return &Router{runtime: runtime, selectors: selectors}, nil
}

const maxGenerateTargets = 8

func (r *Router) Generate(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateResponse, Trace, error) {
	snapshot := request.Clone()
	decision, err := r.selectGenerate(ctx, snapshot)
	if err != nil {
		return inference.GenerateResponse{}, Trace{}, err
	}
	trace := Trace{Decision: decision}
	target := decision.Selected
	trigger := AttemptTriggerSelection
	seen := map[inference.ModelRef]struct{}{target: {}}
	for {
		if _, err := r.runtime.ExplainGenerate(ctx, target, snapshot); err != nil {
			attempt := failedGenerateAttempt(target, AttemptPhasePreflight, trigger, err)
			trace.Attempts = append(trace.Attempts, attempt)
			next, ok, fallbackErr := r.nextGenerateTarget(
				ctx, snapshot, attempt, &trace, seen,
			)
			if fallbackErr != nil {
				return inference.GenerateResponse{}, trace, fallbackErr
			}
			if !ok {
				return inference.GenerateResponse{}, trace, err
			}
			target, trigger = next, AttemptTriggerFallback
			continue
		}
		trace.Attempts = append(trace.Attempts, Attempt{
			Target: target, Phase: AttemptPhasePreflight, Trigger: trigger,
			Outcome: AttemptOutcomeSucceeded,
		})

		response, err := r.runtime.Generate(ctx, target, snapshot)
		if err != nil {
			attempt := failedGenerateAttempt(target, AttemptPhaseExecute, trigger, err)
			trace.Attempts = append(trace.Attempts, attempt)
			next, ok, fallbackErr := r.nextGenerateTarget(
				ctx, snapshot, attempt, &trace, seen,
			)
			if fallbackErr != nil {
				return inference.GenerateResponse{}, trace, fallbackErr
			}
			if !ok {
				return inference.GenerateResponse{}, trace, err
			}
			target, trigger = next, AttemptTriggerFallback
			continue
		}
		trace.Attempts = append(trace.Attempts, Attempt{
			Target: target, Phase: AttemptPhaseExecute, Trigger: trigger,
			Outcome: AttemptOutcomeSucceeded,
		})
		trace.Executed = target
		if response.Metadata.Operation != inference.OperationGenerate ||
			response.Metadata.Model != target.ID {
			return inference.GenerateResponse{}, trace, NewError(
				SelectorContractViolation,
				inference.OperationGenerate,
				errors.New("inference response does not match selected route"),
			)
		}
		return response, trace, nil
	}
}

func (r *Router) GenerateStream(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateStream, Trace, error) {
	snapshot := request.Clone()
	decision, err := r.selectGenerate(ctx, snapshot)
	if err != nil {
		return nil, Trace{}, err
	}
	trace := Trace{Decision: decision}
	target := decision.Selected
	trigger := AttemptTriggerSelection
	seen := map[inference.ModelRef]struct{}{target: {}}
	for {
		if _, err := r.runtime.ExplainGenerateStream(ctx, target, snapshot); err != nil {
			attempt := failedGenerateAttempt(target, AttemptPhasePreflight, trigger, err)
			trace.Attempts = append(trace.Attempts, attempt)
			next, ok, fallbackErr := r.nextGenerateTarget(
				ctx, snapshot, attempt, &trace, seen,
			)
			if fallbackErr != nil {
				return nil, trace, fallbackErr
			}
			if !ok {
				return nil, trace, err
			}
			target, trigger = next, AttemptTriggerFallback
			continue
		}
		trace.Attempts = append(trace.Attempts, Attempt{
			Target: target, Phase: AttemptPhasePreflight, Trigger: trigger,
			Outcome: AttemptOutcomeSucceeded,
		})

		stream, err := r.runtime.GenerateStream(ctx, target, snapshot)
		if err != nil {
			attempt := failedGenerateAttempt(target, AttemptPhaseStreamOpen, trigger, err)
			trace.Attempts = append(trace.Attempts, attempt)
			next, ok, fallbackErr := r.nextGenerateTarget(
				ctx, snapshot, attempt, &trace, seen,
			)
			if fallbackErr != nil {
				return nil, trace, fallbackErr
			}
			if !ok {
				return nil, trace, err
			}
			target, trigger = next, AttemptTriggerFallback
			continue
		}
		trace.Attempts = append(trace.Attempts, Attempt{
			Target: target, Phase: AttemptPhaseStreamOpen, Trigger: trigger,
			Outcome: AttemptOutcomeOpened,
		})
		trace.Executed = target
		return &observableGenerateStream{
			GenerateStream: stream,
			attempt:        &trace.Attempts[len(trace.Attempts)-1],
		}, trace, nil
	}
}

type observableGenerateStream struct {
	inference.GenerateStream
	attempt *Attempt
	once    sync.Once
}

func (s *observableGenerateStream) Next(
	ctx context.Context,
) (inference.GenerateStreamEvent, error) {
	event, err := s.GenerateStream.Next(ctx)
	if err == nil {
		s.once.Do(func() {
			s.attempt.ObservableOutput = true
		})
	}
	return event, err
}

// ExplainGenerate explains the selected target's unary Generate compilation.
func (r *Router) ExplainGenerate(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.Explanation, Decision, error) {
	snapshot := request.Clone()
	decision, err := r.selectGenerate(ctx, snapshot)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.runtime.ExplainGenerate(ctx, decision.Selected, snapshot)
	return explanation, decision, err
}

func (r *Router) Embed(
	ctx context.Context,
	request inference.EmbedRequest,
) (inference.EmbedResponse, Trace, error) {
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationEmbed,
		request,
		inference.EmbedRequest.Clone,
		inference.EmbedRequest.Validate,
		r.selectors.Embed,
		func(
			ctx context.Context,
			selector EmbedSelector,
			request inference.EmbedRequest,
		) (Decision, error) {
			return selector.SelectEmbed(ctx, request)
		},
	)
	if err != nil {
		return inference.EmbedResponse{}, Trace{}, err
	}
	response, err := r.runtime.Embed(ctx, decision.Selected, request)
	if err != nil {
		return inference.EmbedResponse{}, Trace{}, err
	}
	trace, err := traceForResponse(decision, response.Metadata)
	if err != nil {
		return inference.EmbedResponse{}, Trace{}, err
	}
	return response, trace, nil
}

func (r *Router) ExplainEmbed(
	ctx context.Context,
	request inference.EmbedRequest,
) (inference.Explanation, Decision, error) {
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationEmbed,
		request,
		inference.EmbedRequest.Clone,
		inference.EmbedRequest.Validate,
		r.selectors.Embed,
		func(
			ctx context.Context,
			selector EmbedSelector,
			request inference.EmbedRequest,
		) (Decision, error) {
			return selector.SelectEmbed(ctx, request)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.runtime.ExplainEmbed(ctx, decision.Selected, request)
	return explanation, decision, err
}

func (r *Router) Transcribe(
	ctx context.Context,
	request inference.TranscriptionRequest,
) (inference.TranscriptionResponse, Trace, error) {
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationTranscription,
		request,
		inference.TranscriptionRequest.Clone,
		inference.TranscriptionRequest.Validate,
		r.selectors.Transcription,
		func(
			ctx context.Context,
			selector TranscriptionSelector,
			request inference.TranscriptionRequest,
		) (Decision, error) {
			return selector.SelectTranscription(ctx, request)
		},
	)
	if err != nil {
		return inference.TranscriptionResponse{}, Trace{}, err
	}
	response, err := r.runtime.Transcribe(ctx, decision.Selected, request)
	if err != nil {
		return inference.TranscriptionResponse{}, Trace{}, err
	}
	trace, err := traceForResponse(decision, response.Metadata)
	if err != nil {
		return inference.TranscriptionResponse{}, Trace{}, err
	}
	return response, trace, nil
}

func (r *Router) ExplainTranscription(
	ctx context.Context,
	request inference.TranscriptionRequest,
) (inference.Explanation, Decision, error) {
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationTranscription,
		request,
		inference.TranscriptionRequest.Clone,
		inference.TranscriptionRequest.Validate,
		r.selectors.Transcription,
		func(
			ctx context.Context,
			selector TranscriptionSelector,
			request inference.TranscriptionRequest,
		) (Decision, error) {
			return selector.SelectTranscription(ctx, request)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.runtime.ExplainTranscription(ctx, decision.Selected, request)
	return explanation, decision, err
}

func (r *Router) OpenTranscription(
	ctx context.Context,
	config inference.TranscriptionSessionConfig,
) (inference.OpenedTranscriptionSession, Trace, error) {
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationTranscription,
		config,
		inference.TranscriptionSessionConfig.Clone,
		inference.TranscriptionSessionConfig.Validate,
		r.selectors.TranscriptionSession,
		func(
			ctx context.Context,
			selector TranscriptionSessionSelector,
			config inference.TranscriptionSessionConfig,
		) (Decision, error) {
			return selector.SelectTranscriptionSession(ctx, config)
		},
	)
	if err != nil {
		return nil, Trace{}, err
	}
	session, err := r.runtime.OpenTranscription(ctx, decision.Selected, config)
	if err != nil {
		return nil, Trace{}, err
	}
	return session, traceForSession(decision), nil
}

func (r *Router) ExplainTranscriptionSession(
	ctx context.Context,
	config inference.TranscriptionSessionConfig,
) (inference.Explanation, Decision, error) {
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationTranscription,
		config,
		inference.TranscriptionSessionConfig.Clone,
		inference.TranscriptionSessionConfig.Validate,
		r.selectors.TranscriptionSession,
		func(
			ctx context.Context,
			selector TranscriptionSessionSelector,
			config inference.TranscriptionSessionConfig,
		) (Decision, error) {
			return selector.SelectTranscriptionSession(ctx, config)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.runtime.ExplainTranscriptionSession(
		ctx,
		decision.Selected,
		config,
	)
	return explanation, decision, err
}

func (r *Router) OpenRealtime(
	ctx context.Context,
	config inference.RealtimeConfig,
) (inference.OpenedRealtimeSession, Trace, error) {
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationRealtime,
		config,
		inference.RealtimeConfig.Clone,
		inference.RealtimeConfig.Validate,
		r.selectors.Realtime,
		func(
			ctx context.Context,
			selector RealtimeSelector,
			config inference.RealtimeConfig,
		) (Decision, error) {
			return selector.SelectRealtime(ctx, config)
		},
	)
	if err != nil {
		return nil, Trace{}, err
	}
	session, err := r.runtime.OpenRealtime(ctx, decision.Selected, config)
	if err != nil {
		return nil, Trace{}, err
	}
	return session, traceForSession(decision), nil
}

func (r *Router) ExplainRealtime(
	ctx context.Context,
	config inference.RealtimeConfig,
) (inference.Explanation, Decision, error) {
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationRealtime,
		config,
		inference.RealtimeConfig.Clone,
		inference.RealtimeConfig.Validate,
		r.selectors.Realtime,
		func(
			ctx context.Context,
			selector RealtimeSelector,
			config inference.RealtimeConfig,
		) (Decision, error) {
			return selector.SelectRealtime(ctx, config)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.runtime.ExplainRealtime(ctx, decision.Selected, config)
	return explanation, decision, err
}

func (r *Router) selectGenerate(
	ctx context.Context,
	request inference.GenerateRequest,
) (Decision, error) {
	if err := request.Validate(); err != nil {
		return Decision{}, NewError(InvalidRequest, inference.OperationGenerate, err)
	}
	if isNilInterface(r.selectors.Generate) {
		return Decision{}, NewError(
			SelectorUnavailable,
			inference.OperationGenerate,
			errors.New("selector is not configured"),
		)
	}
	decision, err := r.selectors.Generate.SelectGenerate(ctx, request.Clone())
	if err != nil {
		var routeErr *Error
		if errors.As(err, &routeErr) {
			return Decision{}, err
		}
		return Decision{}, NewError(SelectionFailed, inference.OperationGenerate, err)
	}
	if err := decision.ValidateFor(inference.OperationGenerate); err != nil {
		return Decision{}, NewError(
			SelectorContractViolation,
			inference.OperationGenerate,
			err,
		)
	}
	descriptor, err := r.runtime.InspectModel(decision.Selected)
	if err != nil {
		return Decision{}, NewError(SelectionFailed, inference.OperationGenerate, err)
	}
	if !supportsOperation(descriptor, inference.OperationGenerate) {
		return Decision{}, NewError(
			SelectorContractViolation,
			inference.OperationGenerate,
			errors.New("selector returned a model without generate operation"),
		)
	}
	if descriptor.Lifecycle.Status == inference.ModelStatusRetired {
		return Decision{}, NewError(
			SelectorContractViolation,
			inference.OperationGenerate,
			errors.New("selector returned a retired model"),
		)
	}
	return decision, nil
}

func (r *Router) nextGenerateTarget(
	ctx context.Context,
	request inference.GenerateRequest,
	attempt Attempt,
	trace *Trace,
	seen map[inference.ModelRef]struct{},
) (inference.ModelRef, bool, error) {
	if !generateFallbackEligible(attempt) ||
		isNilInterface(r.selectors.GenerateFallback) {
		return inference.ModelRef{}, false, nil
	}
	next, ok, err := r.selectors.GenerateFallback.NextGenerate(
		ctx,
		request.Clone(),
		attempt,
	)
	if err != nil {
		var routeErr *Error
		if errors.As(err, &routeErr) {
			return inference.ModelRef{}, false, err
		}
		return inference.ModelRef{}, false, NewError(
			FallbackFailed,
			inference.OperationGenerate,
			err,
		)
	}
	if !ok {
		if next != (inference.ModelRef{}) {
			return inference.ModelRef{}, false, NewError(
				FallbackContractViolation,
				inference.OperationGenerate,
				errors.New("fallback stop returned a target"),
			)
		}
		return inference.ModelRef{}, false, nil
	}
	if len(seen) >= maxGenerateTargets {
		return inference.ModelRef{}, false, NewError(
			FallbackLimitExceeded,
			inference.OperationGenerate,
			fmt.Errorf("generate fallback exceeds %d targets", maxGenerateTargets),
		)
	}
	if err := next.Validate(); err != nil {
		return inference.ModelRef{}, false, NewError(
			FallbackContractViolation,
			inference.OperationGenerate,
			fmt.Errorf("invalid fallback target: %w", err),
		)
	}
	if _, duplicate := seen[next]; duplicate {
		return inference.ModelRef{}, false, NewError(
			FallbackContractViolation,
			inference.OperationGenerate,
			errors.New("fallback returned a previously attempted target"),
		)
	}
	descriptor, err := r.runtime.InspectModel(next)
	if err != nil {
		return inference.ModelRef{}, false, NewError(
			FallbackContractViolation,
			inference.OperationGenerate,
			err,
		)
	}
	if !supportsOperation(descriptor, inference.OperationGenerate) {
		return inference.ModelRef{}, false, NewError(
			FallbackContractViolation,
			inference.OperationGenerate,
			errors.New("fallback returned a model without generate operation"),
		)
	}
	if descriptor.Lifecycle.Status == inference.ModelStatusRetired {
		return inference.ModelRef{}, false, NewError(
			FallbackContractViolation,
			inference.OperationGenerate,
			errors.New("fallback returned a retired model"),
		)
	}
	seen[next] = struct{}{}
	trace.Fallbacks = append(trace.Fallbacks, FallbackHop{
		From: attempt.Target, To: next, Reason: string(attempt.ErrorKind),
	})
	return next, true, nil
}

func failedGenerateAttempt(
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

func generateFallbackEligible(attempt Attempt) bool {
	if attempt.Outcome != AttemptOutcomeFailed || attempt.ObservableOutput {
		return false
	}
	return fallbackEligibleKind(attempt.ErrorKind)
}

func selectTarget[Request any, Selector any](
	ctx context.Context,
	runtime *inference.Runtime,
	operation inference.Operation,
	request Request,
	clone func(Request) Request,
	validate func(Request) error,
	selector Selector,
	selectRequest func(context.Context, Selector, Request) (Decision, error),
) (Decision, error) {
	snapshot := clone(request)
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
	decision, err := selectRequest(ctx, selector, snapshot)
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

func traceForResponse(
	decision Decision,
	metadata inference.Metadata,
) (Trace, error) {
	if metadata.Operation != decision.Operation ||
		metadata.Model != decision.Selected.ID {
		return Trace{}, NewError(
			SelectorContractViolation,
			decision.Operation,
			errors.New("inference response does not match selected route"),
		)
	}
	return Trace{
		Decision: decision,
		Executed: decision.Selected,
	}, nil
}

// traceForSession trusts the opened session's route because the Runtime
// derives session metadata from the exact target it resolved.
func traceForSession(decision Decision) Trace {
	return Trace{
		Decision: decision,
		Executed: decision.Selected,
	}
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
