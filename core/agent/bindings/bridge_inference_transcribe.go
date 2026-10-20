package bindings

import "github.com/GizClaw/flowcraft/core/inference"

// The transcribe family: speech recognition, unary and duplex. The
// *_Transcribe entry points are the exact-model/router twins of
// generate, over TranscriptionRequest / TranscriptionSessionRequest.

// transcribe is inference.transcribe: one exact-model transcription.
func (b *inferenceBridge) transcribe(raw any) (any, error) {
	const op = "inference.transcribe"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	ref, req, extensions, err := parseInferenceModelCall[inference.TranscriptionRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	resp, err := b.live.transcribe(b.ctx, ref, req)
	if err != nil {
		return nil, err
	}
	return toScriptJSON(resp, op+" response")
}

// routeTranscribe is inference.routeTranscribe: one router-selected
// transcription, with the route trace attached to the response.
func (b *inferenceBridge) routeTranscribe(raw any) (any, error) {
	const op = "inference.routeTranscribe"
	if err := b.requireRouter(op); err != nil {
		return nil, err
	}
	req, extensions, err := parseInferenceRouteCall[inference.TranscriptionRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	resp, trace, err := b.router.Transcribe(b.ctx, req)
	if err != nil {
		return nil, err
	}
	out, err := toScriptJSON(resp, op+" response")
	if err != nil {
		return nil, err
	}
	return attachTrace(out, trace, op)
}

// explainTranscribe is inference.explainTranscribe: one exact-model
// Assembly.ExplainTranscribe call, with no provider I/O.
func (b *inferenceBridge) explainTranscribe(raw any) (any, error) {
	const op = "inference.explainTranscribe"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	ref, req, extensions, err := parseInferenceModelCall[inference.TranscriptionRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	explanation, err := b.live.explainTranscribe(b.ctx, ref, req)
	if err != nil {
		return nil, err
	}
	return toScriptJSON(explanation, op+" response")
}

// routeExplainTranscribe is inference.routeExplainTranscribe: one router
// ExplainTranscribe call, projected as {explanation, decision, limits}.
func (b *inferenceBridge) routeExplainTranscribe(raw any) (any, error) {
	const op = "inference.routeExplainTranscribe"
	if err := b.requireRouter(op); err != nil {
		return nil, err
	}
	req, extensions, err := parseInferenceRouteCall[inference.TranscriptionRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	explanation, decision, err := b.router.ExplainTranscribe(b.ctx, req)
	if err != nil {
		return nil, err
	}
	return routeExplainResult(b.router, explanation, decision, op)
}

// transcribeSession is inference.transcribeSession: it opens an
// exact-model duplex session and hands the script the session handle.
func (b *inferenceBridge) transcribeSession(raw any) (any, error) {
	const op = "inference.transcribeSession"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	ref, req, extensions, err := parseInferenceModelCall[inference.TranscriptionSessionRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	session, err := b.live.transcribeSession(b.ctx, ref, req)
	if err != nil {
		return nil, err
	}
	return newTranscriptionSessionHandle(b.ctx, session, nil), nil
}

// routeTranscribeSession is inference.routeTranscribeSession: the
// router-selected twin of transcribeSession, whose handle carries the
// route trace on its result.
func (b *inferenceBridge) routeTranscribeSession(raw any) (any, error) {
	const op = "inference.routeTranscribeSession"
	if err := b.requireRouter(op); err != nil {
		return nil, err
	}
	req, extensions, err := parseInferenceRouteCall[inference.TranscriptionSessionRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	session, trace, err := b.router.TranscribeSession(b.ctx, req)
	if err != nil {
		return nil, err
	}
	return newTranscriptionSessionHandle(b.ctx, session, &trace), nil
}
