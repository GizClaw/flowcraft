package inference

import (
	"context"
	"fmt"
	"time"
)

// GenerateStreamDecoder implementations must support concurrent calls.
type GenerateStreamDecoder[RawEvent any] func(
	context.Context,
	RawEvent,
) (GenerateStreamEvent, error)

type GenerateStream interface {
	Next(context.Context) (GenerateStreamEvent, error)
	Result() (GenerateResponse, error)
	Close() error
}

type ProviderStream[RawEvent any] interface {
	Next(context.Context) (RawEvent, error)
	Close() error
}

// ProviderStreamMetadata is an optional capability of ProviderStream.
// Streams that learn a provider-assigned request or response identifier
// before their terminal event (the x-request-id header at open, the first
// chunk id, or response.created) implement it so the runtime can attach the
// identifier to mid-stream and truncated-stream failures. Without it,
// identifiers ride only the terminal finish event and are lost when the
// provider stream ends early.
type ProviderStreamMetadata interface {
	RequestID() string
	ResponseID() string
}

type generateStreamDriver[Wire, RawEvent any] struct {
	pipeline *pipeline[
		GenerateRequest,
		Wire,
		ProviderStream[RawEvent],
		GenerateResponse,
	]
	decode  GenerateStreamDecoder[RawEvent]
	binding *generateCompilerBinding
}

func (*generateStreamDriver[Wire, RawEvent]) inferenceGenerateStreamDriver() {}
func (d *generateStreamDriver[Wire, RawEvent]) generateCompilerBinding() *generateCompilerBinding {
	return d.binding
}

func (d *generateStreamDriver[Wire, RawEvent]) Explain(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, request)
}

func (d *generateStreamDriver[Wire, RawEvent]) Stream(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (GenerateStream, error) {
	prepared, err := d.PrepareStream(ctx, model, request)
	if err != nil {
		return nil, err
	}
	return prepared.Execute(ctx)
}

// PrepareStream compiles one streaming request against model and returns a
// handle that opens the stream without compiling again.
func (d *generateStreamDriver[Wire, RawEvent]) PrepareStream(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (*Prepared[GenerateStream], error) {
	compiled, err := d.pipeline.prepare(ctx, model, request)
	if err != nil {
		return nil, err
	}
	return newPrepared(
		model,
		OperationGenerate,
		compiled.Report,
		func(runCtx context.Context) (GenerateStream, error) {
			raw, err := d.pipeline.transport(runCtx, compiled.Wire)
			if err != nil {
				return nil, streamProviderError(model.ID.Provider, "stream.open", err)
			}
			if isNilValue(raw) {
				return nil, newStreamError(
					"stream.open.nil",
					fmt.Errorf("provider opened a nil generate stream"),
				)
			}
			var meta ProviderStreamMetadata
			if ids, ok := raw.(ProviderStreamMetadata); ok {
				meta = ids
			}
			return &decodedGenerateStream[RawEvent]{
				raw:       raw,
				meta:      meta,
				decode:    d.decode,
				model:     model,
				request:   request.Clone(),
				report:    compiled.Report,
				parts:     make(map[int]*generatePartAccumulator),
				startedAt: time.Now(),
			}, nil
		},
	), nil
}
