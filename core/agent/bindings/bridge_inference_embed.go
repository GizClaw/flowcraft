package bindings

import "github.com/GizClaw/flowcraft/core/inference"

// The embed family: the exact-model/router twins of generate, over
// EmbedRequest.

// embed is inference.embed: one exact-model Embed.
func (b *inferenceBridge) embed(raw any) (any, error) {
	const op = "inference.embed"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	ref, req, err := parseInferenceEmbedCall(raw, b.cfg.extensions, op)
	if err != nil {
		return nil, err
	}
	resp, err := b.live.embed(b.ctx, ref, req)
	if err != nil {
		return nil, err
	}
	return toScriptJSON(resp, op+" response")
}

// routeEmbed is inference.routeEmbed: one router-selected Embed, with the
// route trace attached to the response.
func (b *inferenceBridge) routeEmbed(raw any) (any, error) {
	const op = "inference.routeEmbed"
	if err := b.requireRouter(op); err != nil {
		return nil, err
	}
	req, extensions, err := parseInferenceRouteCall[inference.EmbedRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	resp, trace, err := b.router.Embed(b.ctx, req)
	if err != nil {
		return nil, err
	}
	out, err := toScriptJSON(resp, op+" response")
	if err != nil {
		return nil, err
	}
	return attachTrace(out, trace, op)
}

// explainEmbed is inference.explainEmbed: one exact-model
// Assembly.ExplainEmbed call, with no provider I/O.
func (b *inferenceBridge) explainEmbed(raw any) (any, error) {
	const op = "inference.explainEmbed"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	ref, req, err := parseInferenceEmbedCall(raw, b.cfg.extensions, op)
	if err != nil {
		return nil, err
	}
	explanation, err := b.live.explainEmbed(b.ctx, ref, req)
	if err != nil {
		return nil, err
	}
	return toScriptJSON(explanation, op+" response")
}

// routeExplainEmbed is inference.routeExplainEmbed: one router
// ExplainEmbed call, projected as {explanation, decision, limits}.
func (b *inferenceBridge) routeExplainEmbed(raw any) (any, error) {
	const op = "inference.routeExplainEmbed"
	if err := b.requireRouter(op); err != nil {
		return nil, err
	}
	req, extensions, err := parseInferenceRouteCall[inference.EmbedRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	explanation, decision, err := b.router.ExplainEmbed(b.ctx, req)
	if err != nil {
		return nil, err
	}
	return routeExplainResult(b.router, explanation, decision, op)
}
