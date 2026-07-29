package route

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

type EmbedSelector interface {
	SelectEmbed(context.Context, inference.EmbedRequest) (Decision, error)
}

// EmbedFallbackPolicy chooses another exact target after a structured,
// transport-safe failed Embed attempt. Both the request and attempt are
// snapshots; returning ok=false stops fallback.
type EmbedFallbackPolicy interface {
	NextEmbed(
		context.Context,
		inference.EmbedRequest,
		Attempt,
	) (inference.ModelRef, bool, error)
}

type TranscriptionSelector interface {
	SelectTranscription(context.Context, inference.TranscriptionRequest) (Decision, error)
}

// TranscriptionFallbackPolicy chooses another exact target after a structured,
// transport-safe failed Transcribe attempt.
type TranscriptionFallbackPolicy interface {
	NextTranscription(
		context.Context,
		inference.TranscriptionRequest,
		Attempt,
	) (inference.ModelRef, bool, error)
}

type TranscriptionSessionSelector interface {
	SelectTranscriptionSession(
		context.Context,
		inference.TranscriptionSessionConfig,
	) (Decision, error)
}

// TranscriptionSessionFallbackPolicy chooses another exact target after a
// structured, transport-safe failed OpenTranscription attempt. Fallback only
// exists before the session opens: an opened session never migrates.
type TranscriptionSessionFallbackPolicy interface {
	NextTranscriptionSession(
		context.Context,
		inference.TranscriptionSessionConfig,
		Attempt,
	) (inference.ModelRef, bool, error)
}

type RealtimeSelector interface {
	SelectRealtime(context.Context, inference.RealtimeConfig) (Decision, error)
}

// RealtimeFallbackPolicy chooses another exact target after a structured,
// transport-safe failed OpenRealtime attempt. Fallback only exists before the
// session opens: an opened session never migrates.
type RealtimeFallbackPolicy interface {
	NextRealtime(
		context.Context,
		inference.RealtimeConfig,
		Attempt,
	) (inference.ModelRef, bool, error)
}

func (r *Router) Embed(
	ctx context.Context,
	request inference.EmbedRequest,
) (inference.EmbedResponse, Trace, error) {
	return executeWithFallback(
		ctx,
		r.runtime,
		inference.OperationEmbed,
		request.Clone(),
		inference.EmbedRequest.Clone,
		inference.EmbedRequest.Validate,
		r.selectors.Embed,
		func(ctx context.Context, snapshot inference.EmbedRequest) (Decision, error) {
			return r.selectors.Embed.SelectEmbed(ctx, snapshot)
		},
		embedFallbackNext(r.selectors.EmbedFallback),
		nil,
		func(ctx context.Context, target inference.ModelRef, snapshot inference.EmbedRequest) (inference.EmbedResponse, inference.Metadata, error) {
			response, err := r.runtime.Embed(ctx, target, snapshot)
			return response, response.Metadata, err
		},
	)
}

func (r *Router) ExplainEmbed(
	ctx context.Context,
	request inference.EmbedRequest,
) (inference.Explanation, Decision, error) {
	snapshot := request.Clone()
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationEmbed,
		snapshot,
		inference.EmbedRequest.Clone,
		inference.EmbedRequest.Validate,
		r.selectors.Embed,
		func(ctx context.Context, snapshot inference.EmbedRequest) (Decision, error) {
			return r.selectors.Embed.SelectEmbed(ctx, snapshot)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.runtime.ExplainEmbed(ctx, decision.Selected, snapshot)
	return explanation, decision, err
}

func (r *Router) Transcribe(
	ctx context.Context,
	request inference.TranscriptionRequest,
) (inference.TranscriptionResponse, Trace, error) {
	return executeWithFallback(
		ctx,
		r.runtime,
		inference.OperationTranscription,
		request.Clone(),
		inference.TranscriptionRequest.Clone,
		inference.TranscriptionRequest.Validate,
		r.selectors.Transcription,
		func(ctx context.Context, snapshot inference.TranscriptionRequest) (Decision, error) {
			return r.selectors.Transcription.SelectTranscription(ctx, snapshot)
		},
		transcriptionFallbackNext(r.selectors.TranscriptionFallback),
		nil,
		func(ctx context.Context, target inference.ModelRef, snapshot inference.TranscriptionRequest) (inference.TranscriptionResponse, inference.Metadata, error) {
			response, err := r.runtime.Transcribe(ctx, target, snapshot)
			return response, response.Metadata, err
		},
	)
}

func (r *Router) ExplainTranscription(
	ctx context.Context,
	request inference.TranscriptionRequest,
) (inference.Explanation, Decision, error) {
	snapshot := request.Clone()
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationTranscription,
		snapshot,
		inference.TranscriptionRequest.Clone,
		inference.TranscriptionRequest.Validate,
		r.selectors.Transcription,
		func(ctx context.Context, snapshot inference.TranscriptionRequest) (Decision, error) {
			return r.selectors.Transcription.SelectTranscription(ctx, snapshot)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.runtime.ExplainTranscription(ctx, decision.Selected, snapshot)
	return explanation, decision, err
}

func (r *Router) OpenTranscription(
	ctx context.Context,
	config inference.TranscriptionSessionConfig,
) (inference.OpenedTranscriptionSession, Trace, error) {
	return openSessionWithFallback(
		ctx,
		r.runtime,
		inference.OperationTranscription,
		config.Clone(),
		inference.TranscriptionSessionConfig.Clone,
		inference.TranscriptionSessionConfig.Validate,
		r.selectors.TranscriptionSession,
		func(ctx context.Context, snapshot inference.TranscriptionSessionConfig) (Decision, error) {
			return r.selectors.TranscriptionSession.SelectTranscriptionSession(ctx, snapshot)
		},
		transcriptionSessionFallbackNext(r.selectors.TranscriptionSessionFallback),
		r.runtime.OpenTranscription,
	)
}

func (r *Router) ExplainTranscriptionSession(
	ctx context.Context,
	config inference.TranscriptionSessionConfig,
) (inference.Explanation, Decision, error) {
	snapshot := config.Clone()
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationTranscription,
		snapshot,
		inference.TranscriptionSessionConfig.Clone,
		inference.TranscriptionSessionConfig.Validate,
		r.selectors.TranscriptionSession,
		func(ctx context.Context, snapshot inference.TranscriptionSessionConfig) (Decision, error) {
			return r.selectors.TranscriptionSession.SelectTranscriptionSession(ctx, snapshot)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.runtime.ExplainTranscriptionSession(
		ctx,
		decision.Selected,
		snapshot,
	)
	return explanation, decision, err
}

func (r *Router) OpenRealtime(
	ctx context.Context,
	config inference.RealtimeConfig,
) (inference.OpenedRealtimeSession, Trace, error) {
	return openSessionWithFallback(
		ctx,
		r.runtime,
		inference.OperationRealtime,
		config.Clone(),
		inference.RealtimeConfig.Clone,
		inference.RealtimeConfig.Validate,
		r.selectors.Realtime,
		func(ctx context.Context, snapshot inference.RealtimeConfig) (Decision, error) {
			return r.selectors.Realtime.SelectRealtime(ctx, snapshot)
		},
		realtimeFallbackNext(r.selectors.RealtimeFallback),
		r.runtime.OpenRealtime,
	)
}

func (r *Router) ExplainRealtime(
	ctx context.Context,
	config inference.RealtimeConfig,
) (inference.Explanation, Decision, error) {
	snapshot := config.Clone()
	decision, err := selectTarget(
		ctx,
		r.runtime,
		inference.OperationRealtime,
		snapshot,
		inference.RealtimeConfig.Clone,
		inference.RealtimeConfig.Validate,
		r.selectors.Realtime,
		func(ctx context.Context, snapshot inference.RealtimeConfig) (Decision, error) {
			return r.selectors.Realtime.SelectRealtime(ctx, snapshot)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.runtime.ExplainRealtime(ctx, decision.Selected, snapshot)
	return explanation, decision, err
}

func embedFallbackNext(
	policy EmbedFallbackPolicy,
) func(context.Context, inference.EmbedRequest, Attempt) (inference.ModelRef, bool, error) {
	if isNilInterface(policy) {
		return nil
	}
	return policy.NextEmbed
}

func transcriptionFallbackNext(
	policy TranscriptionFallbackPolicy,
) func(context.Context, inference.TranscriptionRequest, Attempt) (inference.ModelRef, bool, error) {
	if isNilInterface(policy) {
		return nil
	}
	return policy.NextTranscription
}

func transcriptionSessionFallbackNext(
	policy TranscriptionSessionFallbackPolicy,
) func(context.Context, inference.TranscriptionSessionConfig, Attempt) (inference.ModelRef, bool, error) {
	if isNilInterface(policy) {
		return nil
	}
	return policy.NextTranscriptionSession
}

func realtimeFallbackNext(
	policy RealtimeFallbackPolicy,
) func(context.Context, inference.RealtimeConfig, Attempt) (inference.ModelRef, bool, error) {
	if isNilInterface(policy) {
		return nil
	}
	return policy.NextRealtime
}
