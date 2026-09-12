package route

import (
	"context"
	"errors"
	"io"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

type TranscribeSelector interface {
	SelectTranscribe(context.Context, inference.TranscriptionRequest) (Decision, error)
}

// TranscribeFallbackPolicy chooses another exact target after a structured,
// transport-safe failed unary transcription attempt. Both the request and
// attempt are snapshots; returning ok=false stops fallback.
type TranscribeFallbackPolicy interface {
	NextTranscribe(
		context.Context,
		inference.TranscriptionRequest,
		Attempt,
	) (model.ModelRef, bool, error)
}

type TranscriptionSessionSelector interface {
	SelectTranscribeSession(
		context.Context,
		inference.TranscriptionSessionRequest,
	) (Decision, error)
}

// TranscriptionSessionFallbackPolicy chooses another exact target after a
// structured, transport-safe failed session open. Fallback exists only
// before open: once a session opens it belongs to the caller.
type TranscriptionSessionFallbackPolicy interface {
	NextTranscribeSession(
		context.Context,
		inference.TranscriptionSessionRequest,
		Attempt,
	) (model.ModelRef, bool, error)
}

func (r *Router) Transcribe(
	ctx context.Context,
	request inference.TranscriptionRequest,
) (inference.TranscriptionResponse, Trace, error) {
	ctx, span := startRouteSpan(ctx, model.OperationTranscription)
	response, trace, err := runAttempts(r, ctx,
		attemptPlan[inference.TranscriptionRequest, inference.TranscriptionResponse]{
			operation: model.OperationTranscription,
			snapshot:  request.Clone(),
			clone:     inference.TranscriptionRequest.Clone,
			validate:  inference.TranscriptionRequest.Validate,
			selector:  r.selectors.Transcribe,
			selectRequest: func(
				ctx context.Context, snapshot inference.TranscriptionRequest,
			) (Decision, error) {
				return r.selectors.Transcribe.SelectTranscribe(ctx, snapshot)
			},
			fallbackNext: transcribeFallbackNext(r.selectors.TranscribeFallback),
			work: func(
				ctx context.Context,
				target model.ModelRef,
				snapshot inference.TranscriptionRequest,
				_ *inference.Prepared[inference.TranscriptionResponse],
			) (inference.TranscriptionResponse, inference.Metadata, error) {
				response, err := r.target.Transcribe(ctx, target, snapshot)
				return response, response.Metadata, err
			},
			phase:     AttemptPhaseExecute,
			outcome:   AttemptOutcomeSucceeded,
			completed: true,
		})
	recordRoute(ctx, span, model.OperationTranscription, trace, response.Metadata, err)
	return response, trace, err
}

func (r *Router) ExplainTranscribe(
	ctx context.Context,
	request inference.TranscriptionRequest,
) (inference.Explanation, Decision, error) {
	snapshot := request.Clone()
	decision, err := selectTarget(
		ctx,
		r.target,
		model.OperationTranscription,
		snapshot,
		inference.TranscriptionRequest.Clone,
		inference.TranscriptionRequest.Validate,
		r.selectors.Transcribe,
		func(ctx context.Context, snapshot inference.TranscriptionRequest) (Decision, error) {
			return r.selectors.Transcribe.SelectTranscribe(ctx, snapshot)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.target.ExplainTranscribe(
		ctx,
		decision.Selected,
		snapshot,
	)
	return explanation, decision, err
}

func (r *Router) TranscribeSession(
	ctx context.Context,
	request inference.TranscriptionSessionRequest,
) (session inference.TranscriptionSession, routeTrace Trace, err error) {
	ctx, span := startRouteSpan(ctx, model.OperationTranscription)
	snapshot := request.Clone()
	session, routeTrace, err = runAttempts(r, ctx,
		attemptPlan[inference.TranscriptionSessionRequest, inference.TranscriptionSession]{
			operation: model.OperationTranscription,
			snapshot:  snapshot,
			clone:     inference.TranscriptionSessionRequest.Clone,
			validate:  inference.TranscriptionSessionRequest.Validate,
			selector:  r.selectors.TranscribeSession,
			selectRequest: func(
				ctx context.Context, snapshot inference.TranscriptionSessionRequest,
			) (Decision, error) {
				return r.selectors.TranscribeSession.SelectTranscribeSession(ctx, snapshot)
			},
			fallbackNext: transcribeSessionFallbackNext(r.selectors.TranscribeSessionFallback),
			prepare: func(
				ctx context.Context,
				target model.ModelRef,
				snapshot inference.TranscriptionSessionRequest,
			) (*inference.Prepared[inference.TranscriptionSession], error) {
				return r.target.PrepareTranscribeSession(ctx, target, snapshot)
			},
			work: func(
				ctx context.Context,
				target model.ModelRef,
				snapshot inference.TranscriptionSessionRequest,
				prepared *inference.Prepared[inference.TranscriptionSession],
			) (inference.TranscriptionSession, inference.Metadata, error) {
				session, err := prepared.Execute(ctx)
				return session, inference.Metadata{}, err
			},
			phase:   AttemptPhaseOpen,
			outcome: AttemptOutcomeOpened,
		})
	if err != nil {
		recordRoute(
			ctx, span, model.OperationTranscription, routeTrace,
			inference.Metadata{}, err,
		)
		return session, routeTrace, err
	}
	session = wrapRouteTranscriptionSession(
		ctx, span, model.OperationTranscription, routeTrace, session)
	return session, routeTrace, err
}

// TranscribeStream opens a routed transcription session, feeds the live
// part stream into it, drains the session to EOF, and returns the final
// transcript with the route trace. It is the one-shot form of
// [Router.TranscribeSession] plus [inference.FeedTranscription]; callers
// that want partial events as they arrive use the session directly.
func (r *Router) TranscribeStream(
	ctx context.Context,
	request inference.TranscriptionSessionRequest,
	stream message.Stream,
) (inference.TranscriptionResponse, Trace, error) {
	session, routeTrace, err := r.TranscribeSession(ctx, request)
	if err != nil {
		return inference.TranscriptionResponse{}, routeTrace, err
	}
	if err := inference.FeedTranscription(
		ctx, session, request.InputFormat, stream,
	); err != nil {
		return inference.TranscriptionResponse{}, routeTrace, err
	}
	for {
		if _, err := session.Next(ctx); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return inference.TranscriptionResponse{}, routeTrace, err
		}
	}
	response, err := session.Result()
	return response, routeTrace, err
}

// ExplainTranscribeSession compiles the selected target's transcription
// session request without provider I/O.
func (r *Router) ExplainTranscribeSession(
	ctx context.Context,
	request inference.TranscriptionSessionRequest,
) (inference.Explanation, Decision, error) {
	snapshot := request.Clone()
	decision, err := selectTarget(
		ctx,
		r.target,
		model.OperationTranscription,
		snapshot,
		inference.TranscriptionSessionRequest.Clone,
		inference.TranscriptionSessionRequest.Validate,
		r.selectors.TranscribeSession,
		func(ctx context.Context, snapshot inference.TranscriptionSessionRequest) (Decision, error) {
			return r.selectors.TranscribeSession.SelectTranscribeSession(ctx, snapshot)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.target.ExplainTranscribeSession(
		ctx,
		decision.Selected,
		snapshot,
	)
	return explanation, decision, err
}

func transcribeFallbackNext(
	policy TranscribeFallbackPolicy,
) func(context.Context, inference.TranscriptionRequest, Attempt) (model.ModelRef, bool, error) {
	if ptr.IsNil(policy) {
		return nil
	}
	return policy.NextTranscribe
}

func transcribeSessionFallbackNext(
	policy TranscriptionSessionFallbackPolicy,
) func(context.Context, inference.TranscriptionSessionRequest, Attempt) (model.ModelRef, bool, error) {
	if ptr.IsNil(policy) {
		return nil
	}
	return policy.NextTranscribeSession
}
