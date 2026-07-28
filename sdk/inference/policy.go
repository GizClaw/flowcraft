package inference

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type (
	RequestDefaults[Req any]  func(context.Context, ModelRef, Req) Req
	RequestTransform[Req any] func(
		context.Context,
		ModelRef,
		Req,
	) (Req, error)
)

// RequestPolicy keeps defaults, deployment constraints, and runtime policy as
// distinct stages. Constraints and Policy may narrow values but cannot add or
// remove active fields.
type RequestPolicy[Req any] struct {
	Defaults    RequestDefaults[Req]
	Constraints RequestTransform[Req]
	Policy      RequestTransform[Req]
}

type RequestPolicies struct {
	Generate             RequestPolicy[GenerateRequest]
	Embed                RequestPolicy[EmbedRequest]
	Transcription        RequestPolicy[TranscriptionRequest]
	TranscriptionSession RequestPolicy[TranscriptionSessionConfig]
	Realtime             RequestPolicy[RealtimeConfig]
}

func WithRequestPolicies(policies RequestPolicies) RuntimeOption {
	return func(options *runtimeOptions) error {
		options.policies = policies
		return nil
	}
}

func applyRequestPolicy[Req any](
	ctx context.Context,
	model ModelRef,
	operation Operation,
	request Req,
	policy RequestPolicy[Req],
	clone func(Req) Req,
	validate func(Req) error,
	activeFields func(Req) []FieldID,
) (Req, error) {
	effective := clone(request)
	if err := validate(effective); err != nil {
		var zero Req
		return zero, NewError(InvalidRequest, operation, "", err)
	}
	if policy.Defaults != nil {
		effective = policy.Defaults(ctx, model, effective)
		if err := validate(effective); err != nil {
			var zero Req
			return zero, NewError(
				CompilerContractViolation,
				operation,
				"",
				fmt.Errorf("defaults produced an invalid request: %w", err),
			)
		}
	}
	var err error
	effective, err = applyNarrowingStage(
		ctx,
		model,
		operation,
		"constraints",
		effective,
		policy.Constraints,
		validate,
		activeFields,
	)
	if err != nil {
		var zero Req
		return zero, err
	}
	effective, err = applyNarrowingStage(
		ctx,
		model,
		operation,
		"policy",
		effective,
		policy.Policy,
		validate,
		activeFields,
	)
	if err != nil {
		var zero Req
		return zero, err
	}
	return effective, nil
}

func applyNarrowingStage[Req any](
	ctx context.Context,
	model ModelRef,
	operation Operation,
	name string,
	request Req,
	transform RequestTransform[Req],
	validate func(Req) error,
	activeFields func(Req) []FieldID,
) (Req, error) {
	if transform == nil {
		return request, nil
	}
	before := activeFields(request)
	transformed, err := transform(ctx, model, request)
	if err != nil {
		var zero Req
		classified := errdefs.FromContext(err)
		if errdefs.IsAborted(classified) || errdefs.IsTimeout(classified) {
			return zero, NewError(OperationInterrupted, operation, "", classified)
		}
		return zero, NewError(PolicyDenied, operation, "", err)
	}
	if err := validate(transformed); err != nil {
		var zero Req
		return zero, NewError(
			PolicyDenied,
			operation,
			"",
			fmt.Errorf("%s produced an invalid request: %w", name, err),
		)
	}
	if !sameFieldSet(before, activeFields(transformed)) {
		var zero Req
		return zero, NewError(
			PolicyDenied,
			operation,
			"",
			fmt.Errorf("%s changed active request fields", name),
		)
	}
	return transformed, nil
}
