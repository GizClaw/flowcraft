package route

import (
	"context"
	"fmt"
	"slices"
	"strings"

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

// nextTarget advances past the attempted target in the effective fallback
// order for the hint. The effective order places the hinted target first and
// keeps every other target in declared order, so a hint elevates one model
// without replacing the default chain: when the hinted target fails, fallback
// restarts at the head of the declared order instead of continuing past the
// hint's position. An attempt target that never came from this policy — a
// custom selector mixed with the policy fallback — stops fallback instead of
// guessing.
func (r *policyRoute) nextTarget(
	hint string,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	order := r.effectiveOrder(hint)
	for index, target := range order {
		if target.model != attempt.Target {
			continue
		}
		if index+1 < len(order) {
			return order[index+1].model, true, nil
		}
		return inference.ModelRef{}, false, nil
	}
	return inference.ModelRef{}, false, nil
}

// effectiveOrder returns the fallback order for one request: when the
// optional model hint names a configured target, that target moves to the
// front and every other target keeps its declared order. Without a hint (or
// when the hint is unknown/ambiguous) the declared order is used unchanged.
// The hinted target is never repeated in the order, so a failed hint cannot
// be re-attempted by fallback.
func (r *policyRoute) effectiveOrder(hint string) []policyTarget {
	if hint == "" {
		return r.targets
	}
	hinted := r.hintTarget(hint)
	if hinted == nil {
		return r.targets
	}
	order := make([]policyTarget, 0, len(r.targets))
	order = append(order, *hinted)
	for _, target := range r.targets {
		if target.model.ID == hinted.model.ID {
			continue
		}
		order = append(order, target)
	}
	return order
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
	if hinted := r.hintTarget(request.ModelHint); hinted != nil {
		supported, err := r.supportsOutputs(hinted.model, requested)
		if err != nil {
			return Decision{}, err
		}
		if supported {
			return Decision{
				Operation: r.operation,
				Tier:      hinted.tier,
				Proposed:  hinted.model,
				Selected:  hinted.model,
			}, nil
		}
		// The hinted target exists but cannot serve this request's output
		// kinds; fall through to the default capability-aware selection.
	}
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

// hintTarget resolves the optional per-call model hint to a configured
// target, or nil when the hint is absent, malformed, or does not name a
// unique configured target:
//
//   - "provider/name" matches the configured model id by provider and name;
//   - a bare "name" matches any configured target with that model name;
//     when several targets share the name the hint is ambiguous and is
//     ignored;
//   - anything else (empty, unknown provider/name, malformed segments)
//     is ignored.
//
// Matching is deliberately profile-blind: the hint selects the model, and
// the deployment's configured profile stays authoritative. A consequence is
// that when one model id is configured under several profiles, a qualified
// hint is ambiguous (all profile entries share the id) and falls back to the
// default selection — a hint cannot distinguish profiles.
func (r *policyRoute) hintTarget(hint string) *policyTarget {
	if hint == "" {
		return nil
	}
	provider, name, qualified := strings.Cut(hint, "/")
	if !qualified {
		name = hint
	}
	var match *policyTarget
	for index := range r.targets {
		target := &r.targets[index]
		if target.model.ID.Name != name {
			continue
		}
		if qualified && target.model.ID.Provider != provider {
			continue
		}
		if match != nil {
			// Ambiguous bare-name hint: no unique configured target.
			return nil
		}
		match = target
	}
	return match
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
	request inference.GenerateRequest,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return r.nextTarget(request.ModelHint, attempt)
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
	return r.nextTarget("", attempt)
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
	return r.nextTarget("", attempt)
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
	return r.nextTarget("", attempt)
}
