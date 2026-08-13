package inference

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

type Compiled[Wire any] struct {
	Wire   Wire
	Report CompileReport
}

// Compiler prepares a provider-native request without provider I/O. It may
// perform local validation and transformation only; Transport owns remote
// execution. This keeps Explain safe for route candidate evaluation. Runtime
// caches bound drivers, so Compiler implementations must support concurrent
// calls.
type Compiler[Req, Wire any] func(
	context.Context,
	ModelRef,
	Req,
) (Compiled[Wire], error)

// Transport is the sole pipeline stage allowed to execute provider I/O.
// Implementations must support concurrent calls.
type Transport[Wire, Raw any] func(context.Context, Wire) (Raw, error)

// Decoder implementations must support concurrent calls.
type Decoder[Raw, Resp any] func(context.Context, Raw) (Resp, error)

type Explanation struct {
	Model     ModelRef
	Operation Operation
	Decisions []Decision
}

type GenerateDriver interface {
	Explain(context.Context, ModelRef, GenerateRequest) (Explanation, error)
	Execute(context.Context, ModelRef, GenerateRequest) (GenerateResponse, error)
	inferenceGenerateDriver()
}

type GenerateStreamDriver interface {
	Explain(context.Context, ModelRef, GenerateRequest) (Explanation, error)
	Stream(context.Context, ModelRef, GenerateRequest) (GenerateStream, error)
	inferenceGenerateStreamDriver()
}

type GenerateOperations struct {
	Unary  GenerateDriver
	Stream GenerateStreamDriver
}

type generateCompilerBinding struct {
	_ byte
}

type boundGenerateDriver interface {
	generateCompilerBinding() *generateCompilerBinding
}

func (o GenerateOperations) Validate() error {
	if isNilValue(o.Unary) && isNilValue(o.Stream) {
		return fmt.Errorf("generate operations require a unary or stream driver")
	}
	if !isNilValue(o.Unary) && !isNilValue(o.Stream) {
		unary, unaryOK := o.Unary.(boundGenerateDriver)
		stream, streamOK := o.Stream.(boundGenerateDriver)
		if !unaryOK || !streamOK ||
			unary.generateCompilerBinding() != stream.generateCompilerBinding() {
			return fmt.Errorf(
				"dual generate operations must be created by BindGenerateOperations",
			)
		}
	}
	return nil
}

type EmbedDriver interface {
	Explain(context.Context, ModelRef, EmbedRequest) (Explanation, error)
	Execute(context.Context, ModelRef, EmbedRequest) (EmbedResponse, error)
	inferenceEmbedDriver()
}

// pipeline binds provider-native stages before registration and remains hidden
// behind an operation-specific driver.
type pipeline[Req, Wire, Raw, Resp any] struct {
	operation        Operation
	compile          Compiler[Req, Wire]
	transport        Transport[Wire, Raw]
	decode           Decoder[Raw, Resp]
	validateRequest  func(Req) error
	activeFields     func(Req) []FieldID
	extensions       func(Req) Extensions
	setExtensions    func(Req, Extensions) Req
	clone            func(Req) Req
	validateResponse func(Req, Resp) error
}

func BindGenerate[Wire, Raw any](
	compile GenerateCompiler[Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, GenerateResponse],
) (GenerateDriver, error) {
	return bindGenerate(
		compile,
		transport,
		decode,
		&generateCompilerBinding{},
	)
}

func bindGenerate[Wire, Raw any](
	compile GenerateCompiler[Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, GenerateResponse],
	binding *generateCompilerBinding,
) (GenerateDriver, error) {
	shapedCompile := bindGenerateCompiler(compile, GenerateExecutionUnary)
	bound, err := bindPipeline(
		OperationGenerate, shapedCompile, transport, decode,
		GenerateRequest.Validate,
		func(request GenerateRequest) []FieldID {
			return request.ActiveFieldsFor(GenerateExecutionUnary)
		},
		func(request GenerateRequest) Extensions { return request.Extensions },
		func(request GenerateRequest, extensions Extensions) GenerateRequest {
			request.Extensions = extensions
			return request
		},
		GenerateRequest.Clone,
		func(request GenerateRequest, response GenerateResponse) error {
			return response.ValidateFor(request)
		},
	)
	if err != nil {
		return nil, err
	}
	return &generateDriver[Wire, Raw]{pipeline: bound, binding: binding}, nil
}

func BindGenerateOperations[Wire, Raw, RawEvent any](
	compile GenerateCompiler[Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, GenerateResponse],
	streamTransport Transport[Wire, ProviderStream[RawEvent]],
	streamDecode GenerateStreamDecoder[RawEvent],
) (GenerateOperations, error) {
	binding := &generateCompilerBinding{}
	unary, err := bindGenerate(compile, transport, decode, binding)
	if err != nil {
		return GenerateOperations{}, err
	}
	stream, err := bindGenerateStream(compile, streamTransport, streamDecode, binding)
	if err != nil {
		return GenerateOperations{}, err
	}
	return GenerateOperations{Unary: unary, Stream: stream}, nil
}

func BindGenerateStream[Wire, RawEvent any](
	compile GenerateCompiler[Wire],
	transport Transport[Wire, ProviderStream[RawEvent]],
	decode GenerateStreamDecoder[RawEvent],
) (GenerateStreamDriver, error) {
	return bindGenerateStream(compile, transport, decode, &generateCompilerBinding{})
}

func bindGenerateStream[Wire, RawEvent any](
	compile GenerateCompiler[Wire],
	transport Transport[Wire, ProviderStream[RawEvent]],
	decode GenerateStreamDecoder[RawEvent],
	binding *generateCompilerBinding,
) (GenerateStreamDriver, error) {
	if decode == nil {
		return nil, errdefs.Validationf("inference generate stream pipeline requires a decoder")
	}
	shapedCompile := bindGenerateCompiler(compile, GenerateExecutionStream)
	bound, err := bindPipeline(
		OperationGenerate, shapedCompile, transport,
		func(context.Context, ProviderStream[RawEvent]) (GenerateResponse, error) {
			return GenerateResponse{}, fmt.Errorf("stream pipeline cannot use unary decode")
		},
		GenerateRequest.Validate,
		func(request GenerateRequest) []FieldID {
			return request.ActiveFieldsFor(GenerateExecutionStream)
		},
		func(request GenerateRequest) Extensions { return request.Extensions },
		func(request GenerateRequest, extensions Extensions) GenerateRequest {
			request.Extensions = extensions
			return request
		},
		GenerateRequest.Clone,
		func(request GenerateRequest, response GenerateResponse) error {
			return response.ValidateFor(request)
		},
	)
	if err != nil {
		return nil, err
	}
	return &generateStreamDriver[Wire, RawEvent]{
		pipeline: bound,
		decode:   decode,
		binding:  binding,
	}, nil
}

func bindGenerateCompiler[Wire any](
	compile GenerateCompiler[Wire],
	shape GenerateExecutionShape,
) Compiler[GenerateRequest, Wire] {
	if compile == nil {
		return nil
	}
	return func(
		ctx context.Context,
		model ModelRef,
		request GenerateRequest,
	) (Compiled[Wire], error) {
		return compile(ctx, model, request, shape)
	}
}

func BindEmbed[Wire, Raw any](
	compile Compiler[EmbedRequest, Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, EmbedResponse],
) (EmbedDriver, error) {
	bound, err := bindPipeline(
		OperationEmbed, compile, transport, decode,
		EmbedRequest.Validate, EmbedRequest.ActiveFields,
		func(request EmbedRequest) Extensions { return request.Extensions },
		func(request EmbedRequest, extensions Extensions) EmbedRequest {
			request.Extensions = extensions
			return request
		},
		EmbedRequest.Clone,
		func(request EmbedRequest, response EmbedResponse) error {
			return response.ValidateFor(request)
		},
	)
	if err != nil {
		return nil, err
	}
	return &embedDriver[Wire, Raw]{pipeline: bound}, nil
}

func typeContainsInterface(value reflect.Type, seen map[reflect.Type]bool) bool {
	if value == nil || seen[value] {
		return false
	}
	seen[value] = true
	if value.Kind() == reflect.Interface {
		return true
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return typeContainsInterface(value.Elem(), seen)
	case reflect.Map:
		return typeContainsInterface(value.Key(), seen) ||
			typeContainsInterface(value.Elem(), seen)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if typeContainsInterface(value.Field(index).Type, seen) {
				return true
			}
		}
	}
	return false
}

func bindPipeline[Req, Wire, Raw, Resp any](
	operation Operation,
	compile Compiler[Req, Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, Resp],
	validateRequest func(Req) error,
	activeFields func(Req) []FieldID,
	extensions func(Req) Extensions,
	setExtensions func(Req, Extensions) Req,
	clone func(Req) Req,
	validateResponse func(Req, Resp) error,
) (*pipeline[Req, Wire, Raw, Resp], error) {
	if compile == nil || transport == nil || decode == nil || setExtensions == nil {
		return nil, errdefs.Validationf("inference pipeline requires all provider stages")
	}
	wireType := reflect.TypeFor[Wire]()
	if invalidProviderWire(wireType) ||
		typeContains(wireType, reflect.TypeFor[Req](), make(map[reflect.Type]bool)) {
		return nil, errdefs.Validationf(
			"provider wire type must be concrete and must not contain canonical or open interface values",
		)
	}
	return &pipeline[Req, Wire, Raw, Resp]{
		operation:        operation,
		compile:          compile,
		transport:        transport,
		decode:           decode,
		validateRequest:  validateRequest,
		activeFields:     activeFields,
		extensions:       extensions,
		setExtensions:    setExtensions,
		clone:            clone,
		validateResponse: validateResponse,
	}, nil
}

func (p *pipeline[Req, Wire, Raw, Resp]) explain(
	ctx context.Context,
	model ModelRef,
	request Req,
) (Explanation, error) {
	compiled, err := p.prepare(ctx, model, request)
	if err != nil {
		return Explanation{}, err
	}
	return Explanation{
		Model:     model,
		Operation: p.operation,
		Decisions: append([]Decision(nil), compiled.Report.Decisions...),
	}, nil
}

func (p *pipeline[Req, Wire, Raw, Resp]) execute(
	ctx context.Context,
	model ModelRef,
	request Req,
) (Resp, CompileReport, error) {
	var zero Resp
	compiled, err := p.prepare(ctx, model, request)
	if err != nil {
		return zero, compiled.Report, err
	}
	raw, err := p.transport(ctx, compiled.Wire)
	if err != nil {
		return zero, compiled.Report, newProviderError(
			p.operation,
			model.ID.Provider,
			err,
		)
	}
	response, err := p.decode(ctx, raw)
	if err != nil {
		return zero, compiled.Report, NewError(InvalidProviderResponse, p.operation, "", err)
	}
	if err := p.validateResponse(request, response); err != nil {
		return zero, compiled.Report, NewError(InvalidProviderResponse, p.operation, "", err)
	}
	return response, compiled.Report, nil
}

func (p *pipeline[Req, Wire, Raw, Resp]) prepare(
	ctx context.Context,
	model ModelRef,
	request Req,
) (Compiled[Wire], error) {
	var zero Compiled[Wire]
	if err := model.Validate(); err != nil {
		return zero, NewError(InvalidRequest, p.operation, "", err)
	}
	if err := p.validateRequest(request); err != nil {
		return zero, NewError(InvalidRequest, p.operation, "", err)
	}
	// Extensions are addressed by ProviderID: on this attempt only the
	// provider's own extensions apply. Foreign extensions are stripped so
	// one request can carry several providers' settings across route
	// fallback; the attempt's ledger then covers only what applied here.
	// The expected field set derives from the caller's request so a lossy
	// clone cannot hide dropped extension fields.
	expected := p.activeFields(p.setExtensions(
		request,
		p.extensions(request).ForProvider(model.ID.Provider),
	))
	snapshot := p.clone(request)
	snapshot = p.setExtensions(
		snapshot,
		p.extensions(snapshot).ForProvider(model.ID.Provider),
	)
	active := append([]FieldID(nil), p.activeFields(snapshot)...)
	if !sameFieldSet(expected, active) {
		return zero, contractViolation(
			p.operation,
			"",
			"request clone changed active fields",
		)
	}
	beforeCompile := p.clone(snapshot)
	compiled, err := p.compile(ctx, model, snapshot)
	if !reflect.DeepEqual(beforeCompile, snapshot) {
		return compiled, contractViolation(
			p.operation,
			"",
			"compiler mutated the canonical request",
		)
	}
	if err != nil {
		classified := errdefs.FromContext(err)
		if errdefs.IsAborted(classified) || errdefs.IsTimeout(classified) {
			return compiled, NewError(
				OperationInterrupted,
				p.operation,
				"",
				classified,
			)
		}
		if reportErr := compiled.Report.ValidateFailure(p.operation, active); reportErr != nil {
			return compiled, reportErr
		}
		var inferenceErr *Error
		if !errors.As(err, &inferenceErr) ||
			inferenceErr.Operation != p.operation ||
			inferenceErr.Field == "" ||
			!inferenceErr.Kind.isCompilerRejection() ||
			!compiled.Report.Rejects(inferenceErr.Field) {
			return compiled, contractViolation(p.operation, "", "compiler returned an invalid structured error")
		}
		return compiled, inferenceErr
	}
	if err := compiled.Report.ValidateSuccess(p.operation, active); err != nil {
		return compiled, err
	}
	return compiled, nil
}

func invalidProviderWire(wire reflect.Type) bool {
	if typeContainsInterface(wire, make(map[reflect.Type]bool)) {
		return true
	}
	for _, canonical := range []reflect.Type{
		reflect.TypeFor[GenerateRequest](),
		reflect.TypeFor[GenerateInput](),
		reflect.TypeFor[message.Message](),
		reflect.TypeFor[message.Content](),
		reflect.TypeFor[InputContent](),
		reflect.TypeFor[Intent](),
		reflect.TypeFor[TextIntent](),
		reflect.TypeFor[ImageIntent](),
		reflect.TypeFor[AudioIntent](),
		reflect.TypeFor[ResponseFormat](),
		reflect.TypeFor[ToolChoice](),
		reflect.TypeFor[message.Part](),
		reflect.TypeFor[message.TextPart](),
		reflect.TypeFor[message.ImagePart](),
		reflect.TypeFor[message.AudioPart](),
		reflect.TypeFor[message.VideoPart](),
		reflect.TypeFor[message.FilePart](),
		reflect.TypeFor[message.DataPart](),
		reflect.TypeFor[message.ToolCallPart](),
		reflect.TypeFor[message.ToolResultPart](),
		reflect.TypeFor[EmbedRequest](),
		reflect.TypeFor[EmbedItem](),
		reflect.TypeFor[media.ImageSize](),
		reflect.TypeFor[media.AudioFormat](),
		reflect.TypeFor[media.VoiceSpec](),
		reflect.TypeFor[media.AudioChunk](),
		reflect.TypeFor[media.VideoFrame](),
		reflect.TypeFor[message.ToolDefinition](),
		reflect.TypeFor[message.ToolCall](),
		reflect.TypeFor[message.ToolResult](),
	} {
		if typeContains(wire, canonical, make(map[reflect.Type]bool)) {
			return true
		}
	}
	return false
}

func sameFieldSet(left, right []FieldID) bool {
	if len(left) != len(right) {
		return false
	}
	fields := make(map[FieldID]struct{}, len(left))
	for _, field := range left {
		fields[field] = struct{}{}
	}
	for _, field := range right {
		if _, ok := fields[field]; !ok {
			return false
		}
	}
	return len(fields) == len(left)
}

func typeContains(container, target reflect.Type, seen map[reflect.Type]bool) bool {
	if container == target {
		return true
	}
	if container == nil || seen[container] {
		return false
	}
	seen[container] = true
	switch container.Kind() {
	case reflect.Array, reflect.Chan, reflect.Pointer, reflect.Slice:
		return typeContains(container.Elem(), target, seen)
	case reflect.Map:
		return typeContains(container.Key(), target, seen) ||
			typeContains(container.Elem(), target, seen)
	case reflect.Struct:
		for i := 0; i < container.NumField(); i++ {
			if typeContains(container.Field(i).Type, target, seen) {
				return true
			}
		}
	}
	return false
}
