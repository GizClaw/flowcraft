package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/backends/plugin/service"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Settings is the strict settings subtree of one RPC-backed provider
// resource.
type Settings struct {
	ID     string   `json:"id"`
	Model  string   `json:"model,omitempty"`
	Models []string `json:"models,omitempty"`
}

// wireRequest is the canonical generate request forwarded to the
// plugin over the RPC channel. Context and Input are pre-serialized at
// compile time so the wire type stays fully concrete.
type wireRequest struct {
	Model   string          `json:"model,omitempty"`
	Context json.RawMessage `json:"context,omitempty"`
	Input   json.RawMessage `json:"input"`
}

// rpcProvider holds the per-instance RPC state shared by the bound
// drivers.
type rpcProvider struct {
	svc    *service.Service
	handle string
}

var _ io.Closer = (*rpcProvider)(nil)

// Close implements io.Closer: it releases the plugin-side resource
// handle via resource.close. Note that [inference.ProviderDefinition]
// is a value frozen by the inference runtime, so deploy's reverse-close
// cannot reach this handle through the resource value; hosts that
// construct the adapter directly close it explicitly, and process
// teardown remains the guaranteed release for runtime-managed providers.
func (p *rpcProvider) Close() error {
	if p.svc == nil {
		return nil
	}
	return p.svc.CloseHandle(context.Background(), p.handle)
}

// providerFactory builds one inference.ProviderDefinition backed by an
// RPC service instance. The plugin process starts lazily on the first
// resource construction.
type providerFactory struct {
	svc  *service.Service
	spec resource.Spec
}

// Spec implements resource.Factory.
func (f providerFactory) Spec() resource.Spec { return f.spec }

// New implements resource.Factory: it constructs the plugin resource
// handle, checks the handshake-declared capability, and binds the
// generate operations to the handle.
func (f providerFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[Settings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validationf("remote provider: settings: %v", err)
	}
	if settings.ID == "" {
		return nil, errdefs.Validationf("remote provider: settings.id is required")
	}
	models := append([]string(nil), settings.Models...)
	if len(models) == 0 && settings.Model != "" {
		models = []string{settings.Model}
	}
	if len(models) == 0 {
		return nil, errdefs.Validationf(
			"remote provider: settings must declare at least one model")
	}

	if err := f.svc.Start(ctx); err != nil {
		return nil, err
	}
	capability, ok := handshakeCapability(f.svc, f.spec.Kind, f.spec.Impl)
	if !ok {
		return nil, errdefs.NotFoundf(
			"remote provider: capability %s/%s not declared by the plugin handshake",
			f.spec.Kind, f.spec.Impl)
	}
	handle, err := f.svc.New(ctx, capabilityKey(f.spec), in.Settings)
	if err != nil {
		return nil, err
	}
	streaming := capability.Streaming &&
		f.svc.Spec().Transport == service.TransportHTTP
	operations, err := bindOperations(f.svc, handle, streaming)
	if err != nil {
		return nil, err
	}

	implementations := make([]inference.ModelImplementation, 0, len(models))
	for _, name := range models {
		implementations = append(implementations, inference.ModelImplementation{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: settings.ID, Name: name},
				Operations: []inference.Operation{
					inference.OperationGenerate,
				},
			},
			Openers: inference.Openers{
				Generate: func(
					context.Context,
					inference.ModelRef,
				) (inference.GenerateOperations, error) {
					return operations, nil
				},
			},
		})
	}
	return inference.ProviderDefinition{ID: settings.ID, Models: implementations}, nil
}

func capabilityKey(spec resource.Spec) string {
	return string(spec.Kind) + "/" + spec.Impl
}

func handshakeCapability(
	svc *service.Service,
	kind resource.Kind,
	impl string,
) (service.Capability, bool) {
	handshake, ok := svc.Handshake()
	if !ok {
		return service.Capability{}, false
	}
	for _, capability := range handshake.Capabilities {
		if capability.Kind == string(kind) && capability.Impl == impl {
			return capability, true
		}
	}
	return service.Capability{}, false
}

func bindOperations(
	svc *service.Service,
	handle string,
	streaming bool,
) (inference.GenerateOperations, error) {
	provider := &rpcProvider{
		svc:    svc,
		handle: handle,
	}
	if !streaming {
		unary, err := inference.BindGenerate[wireRequest, json.RawMessage](
			compileGenerate, provider.unaryTransport, decodeGenerate)
		if err != nil {
			return inference.GenerateOperations{}, errdefs.Validationf(
				"remote provider: bind unary generate: %v", err)
		}
		return inference.GenerateOperations{Unary: unary}, nil
	}
	operations, err := inference.BindGenerateOperations[
		wireRequest, json.RawMessage, wireStreamEvent,
	](
		compileGenerate,
		provider.unaryTransport,
		decodeGenerate,
		provider.streamTransport,
		decodeStreamEvent,
	)
	if err != nil {
		return inference.GenerateOperations{}, errdefs.Validationf(
			"remote provider: bind generate operations: %v", err)
	}
	return operations, nil
}

// compileGenerate forwards every active canonical field natively: the
// plugin receives the complete request and owns interpretation.
func compileGenerate(
	ctx context.Context,
	ref inference.ModelRef,
	req inference.GenerateRequest,
	shape inference.GenerateExecutionShape,
) (inference.Compiled[wireRequest], error) {
	contextRaw, err := json.Marshal(req.Context)
	if err != nil {
		return inference.Compiled[wireRequest]{}, fmt.Errorf(
			"remote provider: encode context: %w", err)
	}
	inputRaw, err := json.Marshal(req.Input)
	if err != nil {
		return inference.Compiled[wireRequest]{}, fmt.Errorf(
			"remote provider: encode input: %w", err)
	}
	active := req.ActiveFieldsFor(shape)
	decisions := make([]inference.Decision, 0, len(active))
	for _, field := range active {
		decisions = append(decisions, inference.Decision{
			Field:       field,
			Disposition: inference.Native,
		})
	}
	return inference.Compiled[wireRequest]{
		Wire: wireRequest{
			Model:   ref.ID.Name,
			Context: contextRaw,
			Input:   inputRaw,
		},
		Report: inference.CompileReport{
			Operation: inference.OperationGenerate,
			Decisions: decisions,
		},
	}, nil
}

func (p *rpcProvider) unaryTransport(
	ctx context.Context,
	wire wireRequest,
) (json.RawMessage, error) {
	args, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("remote provider: encode generate request: %w", err)
	}
	return p.svc.Call(ctx, p.handle, "generate", args)
}

func (p *rpcProvider) streamTransport(
	ctx context.Context,
	wire wireRequest,
) (inference.ProviderStream[wireStreamEvent], error) {
	args, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("remote provider: encode generate request: %w", err)
	}
	spec := p.svc.Spec()
	return openSSE(
		ctx, p.svc.StreamingHTTPClient(),
		spec.URL, spec.Headers, p.handle, "generate_stream", args)
}

func decodeGenerate(
	ctx context.Context,
	raw json.RawMessage,
) (inference.GenerateResponse, error) {
	var response inference.GenerateResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return inference.GenerateResponse{}, fmt.Errorf(
			"remote provider: decode generate response: %w", err)
	}
	return response, nil
}
