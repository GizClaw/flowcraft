package bindings

import "github.com/GizClaw/flowcraft/core/inference"

// The decision family: unary evidence-and-questions calls. There is no
// streaming shape, so each entry point is one exact-model or
// router-selected Decision.

// decision is inference.decision: one exact-model decision.
func (b *inferenceBridge) decision(raw any) (any, error) {
	const op = "inference.decision"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	ref, req, extensions, err := parseInferenceModelCall[inference.DecisionRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	resp, err := b.live.decision(b.ctx, ref, req)
	if err != nil {
		return nil, err
	}
	return toScriptJSON(resp, op+" response")
}

// routeDecision is inference.routeDecision: one router-selected decision,
// with the route trace attached to the response.
func (b *inferenceBridge) routeDecision(raw any) (any, error) {
	const op = "inference.routeDecision"
	if err := b.requireRouter(op); err != nil {
		return nil, err
	}
	req, extensions, err := parseInferenceRouteCall[inference.DecisionRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	resp, trace, err := b.router.Decision(b.ctx, req)
	if err != nil {
		return nil, err
	}
	out, err := toScriptJSON(resp, op+" response")
	if err != nil {
		return nil, err
	}
	return attachTrace(out, trace, op)
}

// explainDecision is inference.explainDecision: one exact-model
// Assembly.ExplainDecision call, with no provider I/O.
func (b *inferenceBridge) explainDecision(raw any) (any, error) {
	const op = "inference.explainDecision"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	ref, req, extensions, err := parseInferenceModelCall[inference.DecisionRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	explanation, err := b.live.explainDecision(b.ctx, ref, req)
	if err != nil {
		return nil, err
	}
	return toScriptJSON(explanation, op+" response")
}

// routeExplainDecision is inference.routeExplainDecision: one router
// ExplainDecision call, projected as {explanation, decision, limits}.
func (b *inferenceBridge) routeExplainDecision(raw any) (any, error) {
	const op = "inference.routeExplainDecision"
	if err := b.requireRouter(op); err != nil {
		return nil, err
	}
	req, extensions, err := parseInferenceRouteCall[inference.DecisionRequest](
		raw,
		b.cfg.extensions,
		op,
	)
	if err != nil {
		return nil, err
	}
	req.Extensions = extensions
	explanation, decision, err := b.router.ExplainDecision(b.ctx, req)
	if err != nil {
		return nil, err
	}
	return routeExplainResult(b.router, explanation, decision, op)
}
