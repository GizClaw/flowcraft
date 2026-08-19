package route

import (
	"context"
	"errors"
	"io"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
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
	) (inference.ModelRef, bool, error)
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
	) (inference.ModelRef, bool, error)
}

func (r *Router) Transcribe(
	ctx context.Context,
	request inference.TranscriptionRequest,
) (inference.TranscriptionResponse, Trace, error) {
	ctx, span := startRouteSpan(ctx, inference.OperationTranscription)
	response, trace, err := executeWithFallback(r,
		ctx,
		inference.OperationTranscription,
		request.Clone(),
		inference.TranscriptionRequest.Clone,
		inference.TranscriptionRequest.Validate,
		r.selectors.Transcribe,
		func(ctx context.Context, snapshot inference.TranscriptionRequest) (Decision, error) {
			return r.selectors.Transcribe.SelectTranscribe(ctx, snapshot)
		},
		transcribeFallbackNext(r.selectors.TranscribeFallback),
		nil,
		func(ctx context.Context, target inference.ModelRef, snapshot inference.TranscriptionRequest) (inference.TranscriptionResponse, inference.Metadata, error) {
			response, err := r.target.Transcribe(ctx, target, snapshot)
			return response, response.Metadata, err
		},
	)
	recordRoute(ctx, span, inference.OperationTranscription, trace, response.Metadata, err)
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
		inference.OperationTranscription,
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
	ctx, span := startRouteSpan(ctx, inference.OperationTranscription)
	snapshot := request.Clone()
	session, routeTrace, err = openSessionWithFallback(r,
		ctx,
		inference.OperationTranscription,
		snapshot,
		inference.TranscriptionSessionRequest.Clone,
		inference.TranscriptionSessionRequest.Validate,
		r.selectors.TranscribeSession,
		func(ctx context.Context, snapshot inference.TranscriptionSessionRequest) (Decision, error) {
			return r.selectors.TranscribeSession.SelectTranscribeSession(ctx, snapshot)
		},
		transcribeSessionFallbackNext(r.selectors.TranscribeSessionFallback),
		func(ctx context.Context, target inference.ModelRef, snapshot inference.TranscriptionSessionRequest) error {
			_, err := r.target.ExplainTranscribeSession(ctx, target, snapshot)
			return err
		},
		func(ctx context.Context, target inference.ModelRef, snapshot inference.TranscriptionSessionRequest) (inference.TranscriptionSession, error) {
			return r.target.TranscribeSession(ctx, target, snapshot)
		},
	)
	if err != nil {
		recordRoute(
			ctx, span, inference.OperationTranscription, routeTrace,
			inference.Metadata{}, err,
		)
		return session, routeTrace, err
	}
	session = wrapRouteTranscriptionSession(
		ctx, span, inference.OperationTranscription, routeTrace, session)
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
		inference.OperationTranscription,
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
) func(context.Context, inference.TranscriptionRequest, Attempt) (inference.ModelRef, bool, error) {
	if isNilInterface(policy) {
		return nil
	}
	return policy.NextTranscribe
}

func transcribeSessionFallbackNext(
	policy TranscriptionSessionFallbackPolicy,
) func(context.Context, inference.TranscriptionSessionRequest, Attempt) (inference.ModelRef, bool, error) {
	if isNilInterface(policy) {
		return nil
	}
	return policy.NextTranscribeSession
}
