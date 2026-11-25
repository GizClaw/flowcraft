package route

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/model"
)

// Generate routes one unary generation.
func (r *Router) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	modes := requiredModes(request)
	ref, err := r.resolve(operationGenerate, request.Model, modes, extensionProviders(request.Extensions))
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	request.Model = ref
	targets, err := acceptContent(r.targets, ref.ID, request.Input.Content)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	return dispatch(ctx, operationGenerate, targets, r.policy.Generate, request,
		modelOfGenerate, withGenerateTarget, invokeGenerate, r.policy.Retry, modes)
}

// GenerateStream routes the opening of one stream.
//
// Failover covers only the open: once the stream is returned, an error arrives
// during iteration, after events have already been produced, and replaying it on
// another target would duplicate partial output. That is also why the route span
// ends when the stream opens: the attempt span follows the stream itself.
func (r *Router) GenerateStream(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	ctx, call := startRoute(ctx, operationGenerate)
	stream, err := r.openStream(ctx, call, request)
	call.finish(err)
	return stream, err
}

// openStream runs the failover-on-open loop without closing the route span.
func (r *Router) openStream(ctx context.Context, call *routeCall, request inference.GenerateRequest) (inference.Stream, error) {
	modes := requiredModes(request)
	ref, err := r.resolve(operationGenerate, request.Model, modes, extensionProviders(request.Extensions))
	if err != nil {
		return nil, err
	}
	request.Model = ref
	eligible, err := acceptContent(r.targets, ref.ID, request.Input.Content)
	if err != nil {
		return nil, err
	}
	candidates := filterTargets(eligible, request.Model, modes)
	ordered := candidates
	if r.policy.Generate != nil {
		selected, err := r.policy.Generate.Select(ctx, request, candidates)
		if err != nil {
			return nil, err
		}
		ordered = selected
	}
	call.selectTargets(ordered)
	if len(ordered) == 0 {
		// Nothing serves the request: say so, rather than returning a nil stream that the
		// caller would read as "opened".
		return nil, inference.NewError(inference.PolicyDenied, inference.FieldModel,
			errors.New("route: no candidate target serves this call"))
	}

	policy := r.policy.Retry.effective(len(ordered))
	trace := TraceFrom(ctx)

	var last error
	tried := 0
	for _, target := range ordered {
		if tried >= policy.MaxTargets {
			break
		}
		tried++
		if tried > 1 {
			call.fallback()
		}
		if target.Provider.Generator == nil {
			continue
		}
		stream, err := target.Provider.GenerateStream(ctx, withGenerateTarget(request, target))
		call.attempt(target, 1, 0, err)
		if err == nil {
			trace.record(Attempt{Target: target.Name})
			return stream, nil
		}
		last = err
		trace.record(Attempt{Target: target.Name, Kind: kindOf(err), Err: err.Error()})
		if classify(err) == RetryStop {
			return nil, err
		}
	}
	return nil, last
}

// requiredModes is what one generation asks the model to do: the shape of the answer it
// wants, plus the tool channel when the request offers tools.
//
// Tools are a mode of their own rather than a flag: a model without the channel must not
// be handed definitions it can never call, and a grant can then narrow a model to text
// only.
func requiredModes(request inference.GenerateRequest) []model.Mode {
	modes := []model.Mode{outputMode(request.Output)}
	if !request.Tools.IsZero() {
		modes = append(modes, model.ModeToolCall)
	}
	return modes
}

// modelOfGenerate is the model the caller asked for, after [Router.resolve] decided it.
func modelOfGenerate(request inference.GenerateRequest) model.ModelRef { return request.Model }

// extensionProviders lists the providers a request carries options for. An option
// names who must answer — the pipeline drops anything addressed elsewhere — so it is
// a constraint on automatic model selection.
func extensionProviders(extensions inference.Extensions) []string {
	if len(extensions) == 0 {
		return nil
	}
	providers := make([]string, 0, len(extensions))
	for _, extension := range extensions {
		if provider := extension.ProviderID(); provider != "" && !slices.Contains(providers, provider) {
			providers = append(providers, provider)
		}
	}
	return providers
}

// withGenerateTarget stamps the chosen target onto the request.
func withGenerateTarget(request inference.GenerateRequest, target Target) inference.GenerateRequest {
	if target.Model.Name != "" {
		request.Model.ID = target.Model
	}
	if request.Model.Profile == "" {
		request.Model.Profile = target.Profile
	}
	return request
}

// invokeGenerate calls one target's generation mode.
func invokeGenerate(ctx context.Context, target Target, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	if target.Provider.Generator == nil {
		return inference.GenerateResponse{}, inference.NewError(
			inference.UnsupportedOperation, inference.FieldModel,
			fmt.Errorf("route: target %q does not generate", target.Name))
	}
	return target.Provider.Generate(ctx, request)
}

// routedGenerator is the generation mode of a Router.
type routedGenerator struct {
	router *Router
}

func (g routedGenerator) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	return g.router.Generate(ctx, request)
}

func (g routedGenerator) GenerateStream(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	return g.router.GenerateStream(ctx, request)
}
