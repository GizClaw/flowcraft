package bindings

import "github.com/GizClaw/flowcraft/core/inference"

// The generate family: one exact-model or router-selected call, and the
// stream/session variants that hand the opened resource back to the
// script instead of completing inside the call.

// generate is inference.generate: one exact-model Generate.
func (b *inferenceBridge) generate(raw any) (any, error) {
	const op = "inference.generate"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	ref, req, err := parseInferenceGenerateCall(raw, b.cfg.extensions, op)
	if err != nil {
		return nil, err
	}
	resp, err := b.live.generate(b.ctx, ref, req)
	if err != nil {
		return nil, err
	}
	return toScriptJSON(resp, op+" response")
}

// route is inference.route: one router-selected Generate, with the
// route trace attached to the response.
func (b *inferenceBridge) route(raw any) (any, error) {
	const op = "inference.route"
	if err := b.requireRouter(op); err != nil {
		return nil, err
	}
	req, extensions, err := parseInferenceRouteCall[inference.GenerateRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	resp, trace, err := b.router.Generate(b.ctx, req)
	if err != nil {
		return nil, err
	}
	out, err := toScriptJSON(resp, op+" response")
	if err != nil {
		return nil, err
	}
	return attachTrace(out, trace, op)
}

// explain is inference.explain: one exact-model Assembly.ExplainGenerate
// call, with no provider I/O.
func (b *inferenceBridge) explain(raw any) (any, error) {
	const op = "inference.explain"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	ref, req, err := parseInferenceGenerateCall(raw, b.cfg.extensions, op)
	if err != nil {
		return nil, err
	}
	explanation, err := b.live.explainGenerate(b.ctx, ref, req)
	if err != nil {
		return nil, err
	}
	return toScriptJSON(explanation, op+" response")
}

// routeExplain is inference.routeExplain: one router ExplainGenerate
// call, projected as {explanation, decision, limits}.
func (b *inferenceBridge) routeExplain(raw any) (any, error) {
	const op = "inference.routeExplain"
	if err := b.requireRouter(op); err != nil {
		return nil, err
	}
	req, extensions, err := parseInferenceRouteCall[inference.GenerateRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	explanation, decision, err := b.router.ExplainGenerate(b.ctx, req)
	if err != nil {
		return nil, err
	}
	return routeExplainResult(b.router, explanation, decision, op)
}

// explainStream is inference.explainStream: local compilation of a
// generate stream, without opening one.
func (b *inferenceBridge) explainStream(raw any) (any, error) {
	const op = "inference.explainStream"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	ref, req, err := parseInferenceGenerateCall(raw, b.cfg.extensions, op)
	if err != nil {
		return nil, err
	}
	explanation, err := b.live.explainGenerateStream(b.ctx, ref, req)
	if err != nil {
		return nil, err
	}
	return toScriptJSON(explanation, op+" response")
}

// routeExplainStream is inference.routeExplainStream: the router-armed
// twin of explainStream.
func (b *inferenceBridge) routeExplainStream(raw any) (any, error) {
	const op = "inference.routeExplainStream"
	if err := b.requireRouter(op); err != nil {
		return nil, err
	}
	req, extensions, err := parseInferenceRouteCall[inference.GenerateRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	explanation, decision, err := b.router.ExplainGenerateStream(b.ctx, req)
	if err != nil {
		return nil, err
	}
	return routeExplainResult(b.router, explanation, decision, op)
}

// stream is inference.stream: it opens an exact-model Generate stream and
// hands the script the iterator handle.
func (b *inferenceBridge) stream(raw any) (any, error) {
	const op = "inference.stream"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	ref, req, err := parseInferenceGenerateCall(raw, b.cfg.extensions, op)
	if err != nil {
		return nil, err
	}
	stream, err := b.live.generateStream(b.ctx, ref, req)
	if err != nil {
		return nil, err
	}
	return newStreamHandle(b.ctx, stream, nil), nil
}

// routeStream is inference.routeStream: the router-selected twin of
// stream, whose handle carries the route trace on its result.
func (b *inferenceBridge) routeStream(raw any) (any, error) {
	const op = "inference.routeStream"
	if err := b.requireRouter(op); err != nil {
		return nil, err
	}
	req, extensions, err := parseInferenceRouteCall[inference.GenerateRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	stream, trace, err := b.router.GenerateStream(b.ctx, req)
	if err != nil {
		return nil, err
	}
	return newStreamHandle(b.ctx, stream, &trace), nil
}
