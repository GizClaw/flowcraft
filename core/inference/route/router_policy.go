package route

import (
	"context"
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// Selectors derives routing behavior from the policy using declared order:
// every selector picks the first compatible target of the operation's pools,
// and every fallback policy advances to the next declared target, crossing
// pool boundaries. Generate selection consults assembly descriptors and skips
// targets whose declared output capabilities cannot serve the request intent;
// Embed/Transcribe selection stays order-based. Repeated model references
// across tiers are collapsed at build time so fallback never returns a
// previously attempted target. Scores remain deployment metadata for custom
// selectors and do not affect this behavior. The Transcription pools serve
// both the unary Transcribe selector and the transcription session selector.
//
// The returned Selectors hold flattened copies of the policy's targets, so
// later mutation of the Policy does not affect routing. An operation with no
// configured pools yields selectors that fail with NoRoute at call time;
// callers are expected to Validate (or ValidateFor) the policy first.
// assembly is the same inference assembly the router executes against; it may
// be nil to disable capability filtering (order-only selection).
func (p Policy) Selectors(assembly *inference.Assembly) Selectors {
	generate := newPolicyRoute(inference.OperationGenerate, p.Generate, assembly)
	embed := newPolicyRoute(inference.OperationEmbed, p.Embed, assembly)
	transcribe := newPolicyRoute(inference.OperationTranscription, p.Transcription, assembly)
	return Selectors{
		Generate:                  generate,
		GenerateFallback:          generate,
		Embed:                     embed,
		EmbedFallback:             embed,
		Transcribe:                transcribe,
		TranscribeFallback:        transcribe,
		TranscribeSession:         transcribe,
		TranscribeSessionFallback: transcribe,
	}
}

// policyRoute implements one operation's selector and fallback policy over the
// policy's declared target order.
type policyRoute struct {
	operation inference.Operation
	targets   []policyTarget
	target    *inference.Assembly
}

type policyTarget struct {
	tier  Tier
	model inference.ModelRef
}

func newPolicyRoute(
	operation inference.Operation,
	pools []Pool,
	assembly *inference.Assembly,
) *policyRoute {
	route := &policyRoute{operation: operation, target: assembly}
	seen := make(map[inference.ModelRef]struct{})
	for _, pool := range pools {
		for _, target := range pool.Targets {
			if _, ok := seen[target.Model]; ok {
				continue
			}
			seen[target.Model] = struct{}{}
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
	_ context.Context,
	request inference.GenerateRequest,
) (Decision, error) {
	if len(r.targets) == 0 {
		return Decision{}, NewError(
			NoRoute,
			r.operation,
			fmt.Errorf("route policy has no %s pools", r.operation),
		)
	}
	requested := request.Input.Content.Intent.OutputKinds()
	for _, target := range r.targets {
		supported, err := r.supportsOutputs(target.model, requested)
		if err != nil {
			return Decision{}, err
		}
		if !supported {
			continue
		}
		return Decision{
			Operation: r.operation,
			Tier:      target.tier,
			Proposed:  target.model,
			Selected:  target.model,
		}, nil
	}
	return Decision{}, NewError(
		NoRoute,
		r.operation,
		fmt.Errorf(
			"no %s pool target declares output kinds %v",
			r.operation,
			requested,
		),
	)
}

// supportsOutputs reports whether the model can serve every requested output
// kind. Targets whose descriptor declares no output capabilities are treated
// as undeclared rather than unsupported: filtering would break deployments
// until every provider publishes capabilities, and preflight remains the
// final arbiter for undeclared models.
func (r *policyRoute) supportsOutputs(
	model inference.ModelRef,
	requested []message.PartKind,
) (bool, error) {
	if r.target == nil || len(requested) == 0 {
		return true, nil
	}
	descriptor, err := r.target.InspectModel(model)
	if err != nil {
		return false, err
	}
	outputs := descriptor.Capabilities.Outputs
	if len(outputs) == 0 {
		return true, nil
	}
	for _, kind := range requested {
		if !slices.Contains(outputs, kind) {
			return false, nil
		}
	}
	return true, nil
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

func (r *policyRoute) SelectTranscribe(
	context.Context,
	inference.TranscriptionRequest,
) (Decision, error) {
	return r.selectTarget()
}

func (r *policyRoute) NextTranscribe(
	_ context.Context,
	_ inference.TranscriptionRequest,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return r.nextTarget(attempt)
}

func (r *policyRoute) SelectTranscribeSession(
	context.Context,
	inference.TranscriptionSessionRequest,
) (Decision, error) {
	return r.selectTarget()
}

func (r *policyRoute) NextTranscribeSession(
	_ context.Context,
	_ inference.TranscriptionSessionRequest,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return r.nextTarget(attempt)
}
