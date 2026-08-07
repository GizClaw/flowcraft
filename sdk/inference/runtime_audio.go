package inference

import (
	"context"
	"fmt"
)

func (r *Runtime) Transcribe(
	ctx context.Context,
	model ModelRef,
	request TranscriptionRequest,
) (resp TranscriptionResponse, err error) {
	ctx, tel := startCall(ctx, OperationTranscription, model, false)
	defer func() {
		if err == nil {
			tel.recordTranscriptionUsage(ctx, resp.Usage)
			tel.recordIDs(ctx, resp.Metadata)
		}
		tel.finish(ctx, err)
	}()
	if _, err := r.resolve(model, OperationTranscription); err != nil {
		return TranscriptionResponse{}, err
	}
	effective, err := applyRequestPolicy(
		ctx, model, OperationTranscription, request, r.policies.Transcription,
		TranscriptionRequest.Clone,
		TranscriptionRequest.Validate,
		TranscriptionRequest.ActiveFields,
	)
	if err != nil {
		return TranscriptionResponse{}, err
	}
	operations, err := r.resolveTranscription(ctx, model)
	if err != nil {
		return TranscriptionResponse{}, err
	}
	if isNilValue(operations.Unary) {
		return TranscriptionResponse{}, unsupportedOperation(
			model,
			OperationTranscription,
			"unary transcription",
		)
	}
	return operations.Unary.Execute(ctx, model, effective)
}

func (r *Runtime) ExplainTranscription(
	ctx context.Context,
	model ModelRef,
	request TranscriptionRequest,
) (Explanation, error) {
	if _, err := r.resolve(model, OperationTranscription); err != nil {
		return Explanation{}, err
	}
	effective, err := applyRequestPolicy(
		ctx, model, OperationTranscription, request, r.policies.Transcription,
		TranscriptionRequest.Clone,
		TranscriptionRequest.Validate,
		TranscriptionRequest.ActiveFields,
	)
	if err != nil {
		return Explanation{}, err
	}
	operations, err := r.resolveTranscription(ctx, model)
	if err != nil {
		return Explanation{}, err
	}
	if isNilValue(operations.Unary) {
		return Explanation{}, unsupportedOperation(
			model,
			OperationTranscription,
			"unary transcription",
		)
	}
	return operations.Unary.Explain(ctx, model, effective)
}

func (r *Runtime) OpenTranscription(
	ctx context.Context,
	model ModelRef,
	config TranscriptionSessionConfig,
) (session OpenedTranscriptionSession, err error) {
	ctx, tel := startCall(ctx, OperationTranscription, model, true)
	defer func() { tel.finish(ctx, err) }()
	if _, err := r.resolve(model, OperationTranscription); err != nil {
		return nil, err
	}
	effective, err := applyRequestPolicy(
		ctx,
		model,
		OperationTranscription,
		config,
		r.policies.TranscriptionSession,
		TranscriptionSessionConfig.Clone,
		TranscriptionSessionConfig.Validate,
		TranscriptionSessionConfig.ActiveFields,
	)
	if err != nil {
		return nil, err
	}
	operations, err := r.resolveTranscription(ctx, model)
	if err != nil {
		return nil, err
	}
	if isNilValue(operations.Session) {
		return nil, unsupportedOperation(
			model,
			OperationTranscription,
			"streaming transcription session",
		)
	}
	opened, metadata, err := operations.Session.Open(ctx, model, effective)
	if err != nil {
		return nil, err
	}
	return wrapTranscriptionSession(model, effective, opened, metadata), nil
}

func (r *Runtime) ExplainTranscriptionSession(
	ctx context.Context,
	model ModelRef,
	config TranscriptionSessionConfig,
) (Explanation, error) {
	if _, err := r.resolve(model, OperationTranscription); err != nil {
		return Explanation{}, err
	}
	effective, err := applyRequestPolicy(
		ctx,
		model,
		OperationTranscription,
		config,
		r.policies.TranscriptionSession,
		TranscriptionSessionConfig.Clone,
		TranscriptionSessionConfig.Validate,
		TranscriptionSessionConfig.ActiveFields,
	)
	if err != nil {
		return Explanation{}, err
	}
	operations, err := r.resolveTranscription(ctx, model)
	if err != nil {
		return Explanation{}, err
	}
	if isNilValue(operations.Session) {
		return Explanation{}, unsupportedOperation(
			model,
			OperationTranscription,
			"streaming transcription session",
		)
	}
	return operations.Session.Explain(ctx, model, effective)
}

func (r *Runtime) OpenRealtime(
	ctx context.Context,
	model ModelRef,
	config RealtimeConfig,
) (session OpenedRealtimeSession, err error) {
	ctx, tel := startCall(ctx, OperationRealtime, model, true)
	defer func() { tel.finish(ctx, err) }()
	if _, err := r.resolve(model, OperationRealtime); err != nil {
		return nil, err
	}
	effective, err := applyRequestPolicy(
		ctx, model, OperationRealtime, config, r.policies.Realtime,
		RealtimeConfig.Clone, RealtimeConfig.Validate, RealtimeConfig.ActiveFields,
	)
	if err != nil {
		return nil, err
	}
	driver, err := r.resolveRealtime(ctx, model)
	if err != nil {
		return nil, err
	}
	opened, metadata, err := driver.Open(ctx, model, effective)
	if err != nil {
		return nil, err
	}
	return wrapRealtimeSession(model, opened, metadata), nil
}

func (r *Runtime) ExplainRealtime(
	ctx context.Context,
	model ModelRef,
	config RealtimeConfig,
) (Explanation, error) {
	if _, err := r.resolve(model, OperationRealtime); err != nil {
		return Explanation{}, err
	}
	effective, err := applyRequestPolicy(
		ctx, model, OperationRealtime, config, r.policies.Realtime,
		RealtimeConfig.Clone, RealtimeConfig.Validate, RealtimeConfig.ActiveFields,
	)
	if err != nil {
		return Explanation{}, err
	}
	driver, err := r.resolveRealtime(ctx, model)
	if err != nil {
		return Explanation{}, err
	}
	return driver.Explain(ctx, model, effective)
}

func (r *Runtime) resolveTranscription(
	ctx context.Context,
	model ModelRef,
) (TranscriptionOperations, error) {
	definition, err := r.resolve(model, OperationTranscription)
	if err != nil {
		return TranscriptionOperations{}, err
	}
	value, err := r.open(
		ctx,
		model,
		OperationTranscription,
		func(openCtx context.Context) (any, error) {
			operations, err := definition.Openers.Transcription(openCtx, model)
			if err != nil {
				return nil, newProviderError(
					OperationTranscription,
					model.ID.Provider,
					err,
				)
			}
			if isNilValue(operations.Unary) && isNilValue(operations.Session) {
				return nil, NewError(
					CompilerContractViolation,
					OperationTranscription,
					"",
					fmt.Errorf("transcription opener returned no drivers"),
				)
			}
			return operations, nil
		},
	)
	if err != nil {
		return TranscriptionOperations{}, err
	}
	operations, ok := value.(TranscriptionOperations)
	if !ok {
		return TranscriptionOperations{}, cacheTypeError(OperationTranscription)
	}
	return operations, nil
}

func (r *Runtime) resolveRealtime(
	ctx context.Context,
	model ModelRef,
) (RealtimeDriver, error) {
	definition, err := r.resolve(model, OperationRealtime)
	if err != nil {
		return nil, err
	}
	value, err := r.open(ctx, model, OperationRealtime, func(openCtx context.Context) (any, error) {
		driver, err := definition.Openers.Realtime(openCtx, model)
		if err != nil {
			return nil, newProviderError(OperationRealtime, model.ID.Provider, err)
		}
		if isNilValue(driver) {
			return nil, NewError(
				CompilerContractViolation,
				OperationRealtime,
				"",
				fmt.Errorf("realtime opener returned a nil driver"),
			)
		}
		return driver, nil
	})
	if err != nil {
		return nil, err
	}
	driver, ok := value.(RealtimeDriver)
	if !ok || isNilValue(driver) {
		return nil, cacheTypeError(OperationRealtime)
	}
	return driver, nil
}
