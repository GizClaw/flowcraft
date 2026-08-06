package route

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

// Selectors derives routing behavior from the policy using declared order:
// every selector picks the first target of the operation's first pool, and
// every fallback policy advances to the next declared target, crossing pool
// boundaries. Scores remain deployment metadata for custom selectors and do
// not affect this behavior. The Transcription pools serve both the unary
// Transcribe selector and the transcription session selector.
//
// The returned Selectors hold flattened copies of the policy's targets, so
// later mutation of the Policy does not affect routing. An operation with no
// configured pools yields selectors that fail with NoRoute at call time;
// callers are expected to Validate (or ValidateFor) the policy first.
func (p Policy) Selectors() Selectors {
	generate := newPolicyRoute(inference.OperationGenerate, p.Generate)
	embed := newPolicyRoute(inference.OperationEmbed, p.Embed)
	transcription := newPolicyRoute(inference.OperationTranscription, p.Transcription)
	realtime := newPolicyRoute(inference.OperationRealtime, p.Realtime)
	return Selectors{
		Generate:                     generate,
		GenerateFallback:             generate,
		Embed:                        embed,
		EmbedFallback:                embed,
		Transcription:                transcription,
		TranscriptionFallback:        transcription,
		TranscriptionSession:         transcription,
		TranscriptionSessionFallback: transcription,
		Realtime:                     realtime,
		RealtimeFallback:             realtime,
	}
}

// policyRoute implements one operation's selector and fallback policy over the
// policy's declared target order.
type policyRoute struct {
	operation inference.Operation
	targets   []policyTarget
}

type policyTarget struct {
	tier  Tier
	model inference.ModelRef
}

func newPolicyRoute(
	operation inference.Operation,
	pools []Pool,
) *policyRoute {
	route := &policyRoute{operation: operation}
	for _, pool := range pools {
		for _, target := range pool.Targets {
			route.targets = append(route.targets, policyTarget{
				tier:  pool.Tier,
				model: target.Model,
			})
		}
	}
	return route
}

func (r *policyRoute) selectTarget() (Decision, error) {
	if len(r.targets) == 0 {
		return Decision{}, NewError(
			NoRoute,
			r.operation,
			fmt.Errorf("route policy has no %s pools", r.operation),
		)
	}
	first := r.targets[0]
	return Decision{
		Operation: r.operation,
		Tier:      first.tier,
		Proposed:  first.model,
		Selected:  first.model,
	}, nil
}

// nextTarget advances past the attempted target in declared order. An attempt
// target that never came from this policy — a custom selector mixed with the
// policy fallback — stops fallback instead of guessing.
func (r *policyRoute) nextTarget(
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	for index, target := range r.targets {
		if target.model != attempt.Target {
			continue
		}
		if index+1 < len(r.targets) {
			return r.targets[index+1].model, true, nil
		}
		return inference.ModelRef{}, false, nil
	}
	return inference.ModelRef{}, false, nil
}

func (r *policyRoute) SelectGenerate(
	context.Context,
	inference.GenerateRequest,
) (Decision, error) {
	return r.selectTarget()
}

func (r *policyRoute) NextGenerate(
	_ context.Context,
	_ inference.GenerateRequest,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return r.nextTarget(attempt)
}

func (r *policyRoute) SelectEmbed(
	context.Context,
	inference.EmbedRequest,
) (Decision, error) {
	return r.selectTarget()
}

func (r *policyRoute) NextEmbed(
	_ context.Context,
	_ inference.EmbedRequest,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return r.nextTarget(attempt)
}

func (r *policyRoute) SelectTranscription(
	context.Context,
	inference.TranscriptionRequest,
) (Decision, error) {
	return r.selectTarget()
}

func (r *policyRoute) NextTranscription(
	_ context.Context,
	_ inference.TranscriptionRequest,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return r.nextTarget(attempt)
}

func (r *policyRoute) SelectTranscriptionSession(
	context.Context,
	inference.TranscriptionSessionConfig,
) (Decision, error) {
	return r.selectTarget()
}

func (r *policyRoute) NextTranscriptionSession(
	_ context.Context,
	_ inference.TranscriptionSessionConfig,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return r.nextTarget(attempt)
}

func (r *policyRoute) SelectRealtime(
	context.Context,
	inference.RealtimeConfig,
) (Decision, error) {
	return r.selectTarget()
}

func (r *policyRoute) NextRealtime(
	_ context.Context,
	_ inference.RealtimeConfig,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return r.nextTarget(attempt)
}
