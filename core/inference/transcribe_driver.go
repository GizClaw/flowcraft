package inference

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// TranscriptionSession is the canonical duplex recognition session: the
// caller feeds audio chunks in and drains partial/final transcript events
// out. Send and Next may be called concurrently. Next returns io.EOF after
// the provider ends the session normally; Result then yields the final
// transcript. Interrupt terminates the session abnormally (barge-in): after
// Interrupt, Send/Next/Result fail with an interruption error and no Result
// is produced. Close ends the session normally without producing a result by
// itself; callers drain Next to EOF first.
type TranscriptionSession interface {
	Send(context.Context, media.AudioChunk) error
	Next(context.Context) (TranscriptionSessionEvent, error)
	Result() (TranscriptionResponse, error)
	Interrupt() error
	Close() error
}

// TranscriptionSessionFinisher is an optional session capability: after the
// caller has no more audio, FinishInput asks the provider to finalize the
// session so draining Next reaches io.EOF and Result yields the accumulated
// transcript. Providers whose wire protocol ends sessions on its own need
// not implement it — callers detect support with a type assertion, and
// calling FinishInput on a session without the capability is a no-op.
type TranscriptionSessionFinisher interface {
	FinishInput(context.Context) error
}

// TranscriptionSessionDecoder converts provider-native session events into
// canonical TranscriptionSessionEvents. Implementations must support
// concurrent calls; sessions serialize decoder use themselves.
type TranscriptionSessionDecoder[RawEvent any] func(
	context.Context,
	RawEvent,
) (TranscriptionSessionEvent, error)

// ProviderSession is the provider-native bidirectional session opened by a
// transcription transport. Send receives canonical chunks and encodes them
// to the provider wire format; Next returns provider-native events.
type ProviderSession[RawEvent any] interface {
	Send(context.Context, media.AudioChunk) error
	Next(context.Context) (RawEvent, error)
	Interrupt() error
	Close() error
}

// TranscriptionSessionTransport opens a provider-native session. It is the
// sole stage allowed to perform provider I/O for sessions; like every
// transport it must support concurrent calls.
type TranscriptionSessionTransport[Wire, RawEvent any] func(
	context.Context,
	Wire,
) (ProviderSession[RawEvent], error)

type TranscriptionDriver interface {
	Explain(context.Context, ModelRef, TranscriptionRequest) (Explanation, error)
	Prepare(context.Context, ModelRef, TranscriptionRequest) (*Prepared[TranscriptionResponse], error)
	Execute(context.Context, ModelRef, TranscriptionRequest) (TranscriptionResponse, error)
	inferenceTranscriptionDriver()
}

type TranscriptionSessionDriver interface {
	Explain(context.Context, ModelRef, TranscriptionSessionRequest) (Explanation, error)
	PrepareSession(context.Context, ModelRef, TranscriptionSessionRequest) (*Prepared[TranscriptionSession], error)
	Open(context.Context, ModelRef, TranscriptionSessionRequest) (TranscriptionSession, error)
	inferenceTranscriptionSessionDriver()
}

// TranscribeOperations materializes the unary and/or duplex session drivers
// a transcription model serves. Openers.Transcribe returns it; the assembly
// routes each execution shape to the matching driver.
type TranscribeOperations struct {
	Unary   TranscriptionDriver
	Session TranscriptionSessionDriver
}

type transcribeCompilerBinding struct {
	_ byte
}

type boundTranscribeDriver interface {
	transcribeCompilerBinding() *transcribeCompilerBinding
}

func (o TranscribeOperations) Validate() error {
	if ptr.IsNil(o.Unary) && ptr.IsNil(o.Session) {
		return fmt.Errorf("transcribe operations require a unary or session driver")
	}
	if !ptr.IsNil(o.Unary) && !ptr.IsNil(o.Session) {
		unary, unaryOK := o.Unary.(boundTranscribeDriver)
		session, sessionOK := o.Session.(boundTranscribeDriver)
		if !unaryOK || !sessionOK ||
			unary.transcribeCompilerBinding() != session.transcribeCompilerBinding() {
			return fmt.Errorf(
				"dual transcribe operations must be created by BindTranscribeOperations",
			)
		}
	}
	return nil
}

// BindTranscribe binds one unary transcription pipeline. compile is local
// validation and wire construction only; transport owns provider I/O.
func BindTranscribe[Wire, Raw any](
	compile Compiler[TranscriptionRequest, Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, TranscriptionResponse],
) (TranscriptionDriver, error) {
	return bindTranscribe(
		compile,
		transport,
		decode,
		&transcribeCompilerBinding{},
	)
}

func bindTranscribe[Wire, Raw any](
	compile Compiler[TranscriptionRequest, Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, TranscriptionResponse],
	binding *transcribeCompilerBinding,
) (TranscriptionDriver, error) {
	bound, err := bindPipeline(
		OperationTranscription,
		compile,
		transport,
		decode,
		TranscriptionRequest.Validate,
		TranscriptionRequest.ActiveFields,
		func(request TranscriptionRequest) Extensions { return request.Extensions },
		func(request TranscriptionRequest, extensions Extensions) TranscriptionRequest {
			request.Extensions = extensions
			return request
		},
		TranscriptionRequest.Clone,
		func(request TranscriptionRequest, response TranscriptionResponse) error {
			return response.ValidateFor(request)
		},
	)
	if err != nil {
		return nil, err
	}
	return &transcribeDriver[Wire, Raw]{
		pipeline: bound,
		binding:  binding,
	}, nil
}

// BindTranscribeSession binds one duplex session driver. The shared pipeline
// compiles and validates the session request exactly like unary
// transcription; the transport opens the provider session and the decoder
// normalizes its events.
func BindTranscribeSession[Wire, RawEvent any](
	compile Compiler[TranscriptionSessionRequest, Wire],
	transport TranscriptionSessionTransport[Wire, RawEvent],
	decode TranscriptionSessionDecoder[RawEvent],
) (TranscriptionSessionDriver, error) {
	return bindTranscribeSession(
		compile,
		transport,
		decode,
		&transcribeCompilerBinding{},
	)
}

func bindTranscribeSession[Wire, RawEvent any](
	compile Compiler[TranscriptionSessionRequest, Wire],
	transport TranscriptionSessionTransport[Wire, RawEvent],
	decode TranscriptionSessionDecoder[RawEvent],
	binding *transcribeCompilerBinding,
) (TranscriptionSessionDriver, error) {
	if decode == nil {
		return nil, errdefs.Validationf(
			"inference transcription session requires a decoder",
		)
	}
	// The session shares the compile/validate pipeline with unary
	// transcription. Its decode stage never runs as a unary decoder:
	// events decode per raw event inside the session, not per response.
	bound, err := bindPipeline(
		OperationTranscription,
		compile,
		Transport[Wire, ProviderSession[RawEvent]](transport),
		func(context.Context, ProviderSession[RawEvent]) (TranscriptionResponse, error) {
			return TranscriptionResponse{}, fmt.Errorf(
				"transcription session has no unary decode",
			)
		},
		TranscriptionSessionRequest.Validate,
		TranscriptionSessionRequest.ActiveFields,
		func(request TranscriptionSessionRequest) Extensions {
			return request.Extensions
		},
		func(request TranscriptionSessionRequest, extensions Extensions) TranscriptionSessionRequest {
			request.Extensions = extensions
			return request
		},
		TranscriptionSessionRequest.Clone,
		func(request TranscriptionSessionRequest, response TranscriptionResponse) error {
			return validateSessionResult(response)
		},
	)
	if err != nil {
		return nil, err
	}
	return &transcribeSessionDriver[Wire, RawEvent]{
		pipeline: bound,
		decode:   decode,
		binding:  binding,
	}, nil
}

// BindTranscribeOperations binds unary and session drivers against the same
// wire family so the runtime can prove both shapes were constructed
// together. The shapes compile different request contracts (a complete
// audio source vs an open-time session request), so each takes its own
// compiler while sharing the Wire type and binding.
func BindTranscribeOperations[Wire, Raw, RawEvent any](
	compile Compiler[TranscriptionRequest, Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, TranscriptionResponse],
	sessionCompile Compiler[TranscriptionSessionRequest, Wire],
	sessionTransport TranscriptionSessionTransport[Wire, RawEvent],
	sessionDecode TranscriptionSessionDecoder[RawEvent],
) (TranscribeOperations, error) {
	binding := &transcribeCompilerBinding{}
	unary, err := bindTranscribe(compile, transport, decode, binding)
	if err != nil {
		return TranscribeOperations{}, err
	}
	session, err := bindTranscribeSession(
		sessionCompile,
		sessionTransport,
		sessionDecode,
		binding,
	)
	if err != nil {
		return TranscribeOperations{}, err
	}
	return TranscribeOperations{Unary: unary, Session: session}, nil
}

type transcribeDriver[Wire, Raw any] struct {
	pipeline *pipeline[TranscriptionRequest, Wire, Raw, TranscriptionResponse]
	binding  *transcribeCompilerBinding
}

func (*transcribeDriver[Wire, Raw]) inferenceTranscriptionDriver() {}
func (d *transcribeDriver[Wire, Raw]) transcribeCompilerBinding() *transcribeCompilerBinding {
	return d.binding
}

func (d *transcribeDriver[Wire, Raw]) Explain(
	ctx context.Context,
	model ModelRef,
	request TranscriptionRequest,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, request)
}

func (d *transcribeDriver[Wire, Raw]) Execute(
	ctx context.Context,
	model ModelRef,
	request TranscriptionRequest,
) (TranscriptionResponse, error) {
	prepared, err := d.Prepare(ctx, model, request)
	if err != nil {
		return TranscriptionResponse{}, err
	}
	return prepared.Execute(ctx)
}

// Prepare compiles one whole-file transcription request against model and
// returns a handle that executes it without compiling again.
func (d *transcribeDriver[Wire, Raw]) Prepare(
	ctx context.Context,
	model ModelRef,
	request TranscriptionRequest,
) (*Prepared[TranscriptionResponse], error) {
	compiled, err := d.pipeline.prepare(ctx, model, request)
	if err != nil {
		return nil, err
	}
	return newPrepared(
		model,
		OperationTranscription,
		compiled.Report,
		func(runCtx context.Context) (TranscriptionResponse, error) {
			start := time.Now()
			response, err := d.pipeline.executeCompiled(
				runCtx, model, request, compiled)
			if err != nil {
				return TranscriptionResponse{}, err
			}
			response.Usage.Model = model
			response.Usage.LatencyMs = time.Since(start).Milliseconds()
			response.Metadata = mergeProviderIDs(
				compiled.Report.Metadata(model),
				response.Metadata,
			)
			return response, nil
		},
	), nil
}

type transcribeSessionDriver[Wire, RawEvent any] struct {
	pipeline *pipeline[
		TranscriptionSessionRequest,
		Wire,
		ProviderSession[RawEvent],
		TranscriptionResponse,
	]
	decode  TranscriptionSessionDecoder[RawEvent]
	binding *transcribeCompilerBinding
}

func (*transcribeSessionDriver[Wire, RawEvent]) inferenceTranscriptionSessionDriver() {}
func (d *transcribeSessionDriver[Wire, RawEvent]) transcribeCompilerBinding() *transcribeCompilerBinding {
	return d.binding
}

func (d *transcribeSessionDriver[Wire, RawEvent]) Explain(
	ctx context.Context,
	model ModelRef,
	request TranscriptionSessionRequest,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, request)
}

func (d *transcribeSessionDriver[Wire, RawEvent]) Open(
	ctx context.Context,
	model ModelRef,
	request TranscriptionSessionRequest,
) (TranscriptionSession, error) {
	prepared, err := d.PrepareSession(ctx, model, request)
	if err != nil {
		return nil, err
	}
	return prepared.Execute(ctx)
}

// PrepareSession compiles one duplex transcription session request against
// model and returns a handle that opens the session without compiling again.
func (d *transcribeSessionDriver[Wire, RawEvent]) PrepareSession(
	ctx context.Context,
	model ModelRef,
	request TranscriptionSessionRequest,
) (*Prepared[TranscriptionSession], error) {
	compiled, err := d.pipeline.prepare(ctx, model, request)
	if err != nil {
		return nil, err
	}
	return newPrepared(
		model,
		OperationTranscription,
		compiled.Report,
		func(runCtx context.Context) (TranscriptionSession, error) {
			raw, err := d.pipeline.transport(runCtx, compiled.Wire)
			if err != nil {
				return nil, newProviderError(
					OperationTranscription,
					model.ID.Provider,
					err,
				)
			}
			if ptr.IsNil(raw) {
				return nil, NewError(
					InvalidProviderResponse,
					OperationTranscription,
					"",
					fmt.Errorf("provider opened a nil transcription session"),
				)
			}
			return &decodedTranscriptionSession[RawEvent]{
				raw:       raw,
				decode:    d.decode,
				model:     model,
				request:   request.Clone(),
				report:    compiled.Report,
				startedAt: time.Now(),
			}, nil
		},
	), nil
}
