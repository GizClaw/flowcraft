package inference

import (
	"context"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"golang.org/x/sync/singleflight"
)

type RuntimeOption func(*runtimeOptions) error

type runtimeOptions struct {
	policies RequestPolicies
}

type driverCacheKey struct {
	model      ModelRef
	operation  Operation
	generation uint64
}

// Invalidation selects opened drivers to evict. Empty fields are wildcards.
type Invalidation struct {
	Provider string
	Model    *ModelID
	// Profile is nil to match every profile. A pointer to "" selects only the
	// provider's default profile.
	Profile *string
}

// Runtime is the instance-owned entry point for all inference operations.
// Provider definitions are immutable after construction.
type Runtime struct {
	registry map[string]providerEntry
	policies RequestPolicies

	mu          sync.Mutex
	generations map[ModelRef]uint64
	cache       map[driverCacheKey]any
	opens       singleflight.Group
}

func NewRuntime(
	definitions []ProviderDefinition,
	options ...RuntimeOption,
) (*Runtime, error) {
	registry, err := buildRegistry(definitions)
	if err != nil {
		return nil, errdefs.Validation(err)
	}
	var configured runtimeOptions
	for index, option := range options {
		if option == nil {
			return nil, errdefs.Validationf("runtime option %d is nil", index)
		}
		if err := option(&configured); err != nil {
			return nil, errdefs.Validationf("runtime option %d: %v", index, err)
		}
	}
	return &Runtime{
		registry:    registry,
		policies:    configured.policies,
		generations: make(map[ModelRef]uint64),
		cache:       make(map[driverCacheKey]any),
	}, nil
}

func (r *Runtime) Models(provider string) ([]ModelDescriptor, error) {
	definition, ok := r.registry[provider]
	if !ok {
		return nil, NewError(
			UnknownProvider,
			"",
			"",
			fmt.Errorf("provider %q is not registered", provider),
		)
	}
	models := make([]ModelDescriptor, len(definition.definition.Models))
	for index, model := range definition.definition.Models {
		models[index] = model.Descriptor.Clone()
	}
	return models, nil
}

// InspectModel validates an exact provider/model/profile target and returns
// immutable descriptive metadata without opening a driver. Selectors can use
// it to reject invalid route targets before compilation or provider I/O.
func (r *Runtime) InspectModel(model ModelRef) (ModelDescriptor, error) {
	definition, err := r.lookup(model, "")
	if err != nil {
		return ModelDescriptor{}, err
	}
	return definition.Descriptor.Clone(), nil
}

func (r *Runtime) Generate(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (resp GenerateResponse, err error) {
	ctx, tel := startCall(ctx, OperationGenerate, model, false)
	defer func() {
		if err == nil {
			tel.stampUsage(&resp.Usage)
			tel.recordUsage(ctx, resp.Usage)
			tel.recordIDs(ctx, resp.Metadata)
		}
		tel.finish(ctx, err)
	}()
	effective, err := r.prepareGenerateRequest(ctx, model, request)
	if err != nil {
		return GenerateResponse{}, err
	}
	operations, err := r.resolveGenerate(ctx, model)
	if err != nil {
		return GenerateResponse{}, err
	}
	if isNilValue(operations.Unary) {
		return GenerateResponse{}, unsupportedOperation(
			model,
			OperationGenerate,
			"unary generation",
		)
	}
	return operations.Unary.Execute(ctx, model, effective)
}

func (r *Runtime) GenerateStream(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (stream GenerateStream, err error) {
	ctx, tel := startCall(ctx, OperationGenerate, model, true)
	defer func() {
		if err != nil {
			// Open failed: the Runtime still owns the span. Once a
			// stream opens, the telemetryStream wrapper takes over.
			tel.finish(ctx, err)
		}
	}()
	effective, err := r.prepareGenerateRequest(ctx, model, request)
	if err != nil {
		return nil, err
	}
	operations, err := r.resolveGenerate(ctx, model)
	if err != nil {
		return nil, err
	}
	if isNilValue(operations.Stream) {
		return nil, unsupportedOperation(
			model,
			OperationGenerate,
			"streaming generation",
		)
	}
	opened, err := operations.Stream.Stream(ctx, model, effective)
	if err != nil {
		return nil, err
	}
	return wrapStreamTelemetry(ctx, tel, opened), nil
}

// ExplainGenerate compiles and explains unary Generate execution without
// provider I/O.
func (r *Runtime) ExplainGenerate(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (Explanation, error) {
	effective, err := r.prepareGenerateRequest(ctx, model, request)
	if err != nil {
		return Explanation{}, err
	}
	operations, err := r.resolveGenerate(ctx, model)
	if err != nil {
		return Explanation{}, err
	}
	if isNilValue(operations.Unary) {
		return Explanation{}, unsupportedOperation(
			model,
			OperationGenerate,
			"unary generation",
		)
	}
	return operations.Unary.Explain(ctx, model, effective)
}

// ExplainGenerateStream compiles and explains streaming Generate execution
// without provider I/O.
func (r *Runtime) ExplainGenerateStream(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (Explanation, error) {
	effective, err := r.prepareGenerateRequest(ctx, model, request)
	if err != nil {
		return Explanation{}, err
	}
	operations, err := r.resolveGenerate(ctx, model)
	if err != nil {
		return Explanation{}, err
	}
	if isNilValue(operations.Stream) {
		return Explanation{}, unsupportedOperation(
			model,
			OperationGenerate,
			"streaming generation",
		)
	}
	return operations.Stream.Explain(ctx, model, effective)
}

func (r *Runtime) Embed(
	ctx context.Context,
	model ModelRef,
	request EmbedRequest,
) (resp EmbedResponse, err error) {
	ctx, tel := startCall(ctx, OperationEmbed, model, false)
	defer func() {
		if err == nil {
			tel.recordEmbedUsage(ctx, resp.Usage)
			tel.recordIDs(ctx, resp.Metadata)
		}
		tel.finish(ctx, err)
	}()
	if _, err := r.resolve(model, OperationEmbed); err != nil {
		return EmbedResponse{}, err
	}
	effective, err := applyRequestPolicy(
		ctx,
		model,
		OperationEmbed,
		request,
		r.policies.Embed,
		EmbedRequest.Clone,
		EmbedRequest.Validate,
		EmbedRequest.ActiveFields,
	)
	if err != nil {
		return EmbedResponse{}, err
	}
	driver, err := r.resolveEmbed(ctx, model)
	if err != nil {
		return EmbedResponse{}, err
	}
	return driver.Execute(ctx, model, effective)
}

func (r *Runtime) ExplainEmbed(
	ctx context.Context,
	model ModelRef,
	request EmbedRequest,
) (Explanation, error) {
	if _, err := r.resolve(model, OperationEmbed); err != nil {
		return Explanation{}, err
	}
	effective, err := applyRequestPolicy(
		ctx,
		model,
		OperationEmbed,
		request,
		r.policies.Embed,
		EmbedRequest.Clone,
		EmbedRequest.Validate,
		EmbedRequest.ActiveFields,
	)
	if err != nil {
		return Explanation{}, err
	}
	driver, err := r.resolveEmbed(ctx, model)
	if err != nil {
		return Explanation{}, err
	}
	return driver.Explain(ctx, model, effective)
}

func (r *Runtime) prepareGenerateRequest(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (GenerateRequest, error) {
	if _, err := r.resolve(model, OperationGenerate); err != nil {
		return GenerateRequest{}, err
	}
	return applyRequestPolicy(
		ctx,
		model,
		OperationGenerate,
		request,
		r.policies.Generate,
		GenerateRequest.Clone,
		GenerateRequest.Validate,
		GenerateRequest.ActiveFields,
	)
}

func (r *Runtime) resolveGenerate(
	ctx context.Context,
	model ModelRef,
) (GenerateOperations, error) {
	definition, err := r.resolve(model, OperationGenerate)
	if err != nil {
		return GenerateOperations{}, err
	}
	value, err := r.open(
		ctx,
		model,
		OperationGenerate,
		func(openCtx context.Context) (any, error) {
			operations, err := definition.Openers.Generate(openCtx, model)
			if err != nil {
				return nil, newProviderError(
					OperationGenerate,
					model.ID.Provider,
					err,
				)
			}
			if err := operations.Validate(); err != nil {
				return nil, NewError(
					CompilerContractViolation,
					OperationGenerate,
					"",
					err,
				)
			}
			return operations, nil
		},
	)
	if err != nil {
		return GenerateOperations{}, err
	}
	operations, ok := value.(GenerateOperations)
	if !ok {
		return GenerateOperations{}, cacheTypeError(OperationGenerate)
	}
	return operations, nil
}

func (r *Runtime) resolveEmbed(
	ctx context.Context,
	model ModelRef,
) (EmbedDriver, error) {
	definition, err := r.resolve(model, OperationEmbed)
	if err != nil {
		return nil, err
	}
	value, err := r.open(ctx, model, OperationEmbed, func(openCtx context.Context) (any, error) {
		driver, err := definition.Openers.Embed(openCtx, model)
		if err != nil {
			return nil, newProviderError(OperationEmbed, model.ID.Provider, err)
		}
		if isNilValue(driver) {
			return nil, NewError(
				CompilerContractViolation,
				OperationEmbed,
				"",
				fmt.Errorf("embed opener returned a nil driver"),
			)
		}
		return driver, nil
	})
	if err != nil {
		return nil, err
	}
	driver, ok := value.(EmbedDriver)
	if !ok || isNilValue(driver) {
		return nil, cacheTypeError(OperationEmbed)
	}
	return driver, nil
}

func (r *Runtime) resolve(
	model ModelRef,
	operation Operation,
) (ModelImplementation, error) {
	definition, err := r.lookup(model, operation)
	if err != nil {
		return ModelImplementation{}, err
	}
	switch operation {
	case OperationGenerate:
		if definition.Openers.Generate == nil {
			return ModelImplementation{}, unsupportedOperation(model, operation, "generation")
		}
	case OperationEmbed:
		if definition.Openers.Embed == nil {
			return ModelImplementation{}, unsupportedOperation(model, operation, "embedding")
		}
	case OperationTranscription:
		if definition.Openers.Transcription == nil {
			return ModelImplementation{}, unsupportedOperation(model, operation, "transcription")
		}
	case OperationRealtime:
		if definition.Openers.Realtime == nil {
			return ModelImplementation{}, unsupportedOperation(model, operation, "realtime")
		}
	default:
		return ModelImplementation{}, NewError(
			UnsupportedOperation,
			operation,
			"",
			fmt.Errorf("unknown operation %q", operation),
		)
	}
	return definition, nil
}

func (r *Runtime) lookup(
	model ModelRef,
	operation Operation,
) (ModelImplementation, error) {
	if err := model.Validate(); err != nil {
		return ModelImplementation{}, NewError(InvalidRequest, operation, "", err)
	}
	provider, ok := r.registry[model.ID.Provider]
	if !ok {
		return ModelImplementation{}, NewError(
			UnknownProvider,
			operation,
			"",
			fmt.Errorf("provider %q is not registered", model.ID.Provider),
		)
	}
	profile, ok := provider.profiles[model.Profile]
	if !ok {
		return ModelImplementation{}, NewError(
			UnknownProfile,
			operation,
			"",
			fmt.Errorf("profile %q is not registered", model.Profile),
		)
	}
	index, ok := provider.models[model.ID.Name]
	var definition ModelImplementation
	if ok {
		definition = provider.definition.Models[index]
	}
	if !ok {
		if provider.definition.Dynamic == nil {
			return ModelImplementation{}, NewError(
				UnknownModel,
				operation,
				"",
				fmt.Errorf("model %q is not registered", model.ID.Name),
			)
		}
		definition = ModelImplementation{
			Descriptor: ModelDescriptor{
				ID:         model.ID,
				Operations: provider.definition.Dynamic.Operations(),
			},
			Openers: *provider.definition.Dynamic,
		}
	}
	if operation == "" {
		definition.Descriptor.Operations = profile.filter(
			definition.Descriptor.Operations,
		)
	} else if !profile.allows(operation) {
		return ModelImplementation{}, NewError(
			UnsupportedOperation,
			operation,
			"",
			fmt.Errorf(
				"profile %q does not support operation %q",
				model.Profile,
				operation,
			),
		)
	}
	return definition, nil
}

func (r *Runtime) open(
	ctx context.Context,
	model ModelRef,
	operation Operation,
	open func(context.Context) (any, error),
) (any, error) {
	r.mu.Lock()
	generation := r.generations[model]
	r.generations[model] = generation
	key := driverCacheKey{
		model:      model,
		operation:  operation,
		generation: generation,
	}
	if value, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return value, nil
	}
	r.mu.Unlock()

	result := r.opens.DoChan(key.singleflightKey(), func() (any, error) {
		r.mu.Lock()
		if value, ok := r.cache[key]; ok {
			r.mu.Unlock()
			return value, nil
		}
		r.mu.Unlock()

		value, err := open(context.Background())
		r.mu.Lock()
		if err == nil && r.generations[model] == generation {
			r.cache[key] = value
		}
		r.mu.Unlock()
		return value, err
	})
	select {
	case <-ctx.Done():
		return nil, NewError(OperationInterrupted, operation, "", ctx.Err())
	case result := <-result:
		return result.Val, result.Err
	}
}

func (r *Runtime) Invalidate(filter Invalidation) error {
	if err := filter.Validate(); err != nil {
		return errdefs.Validation(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	models := make(map[ModelRef]struct{})
	for model := range r.generations {
		if filter.Matches(model) {
			models[model] = struct{}{}
		}
	}
	for key := range r.cache {
		if filter.Matches(key.model) {
			models[key.model] = struct{}{}
			delete(r.cache, key)
		}
	}
	for model := range models {
		r.generations[model]++
	}
	return nil
}

func (filter Invalidation) Validate() error {
	if filter.Provider != "" && !extensionIDPattern.MatchString(filter.Provider) {
		return fmt.Errorf("invalidation has invalid provider %q", filter.Provider)
	}
	if filter.Model != nil {
		if err := filter.Model.Validate(); err != nil {
			return fmt.Errorf("invalidation model: %w", err)
		}
		if filter.Provider != "" && filter.Provider != filter.Model.Provider {
			return fmt.Errorf(
				"invalidation provider %q does not match model provider %q",
				filter.Provider,
				filter.Model.Provider,
			)
		}
	}
	if filter.Profile != nil &&
		*filter.Profile != "" &&
		!extensionIDPattern.MatchString(*filter.Profile) {
		return fmt.Errorf("invalidation has invalid profile %q", *filter.Profile)
	}
	return nil
}

func (filter Invalidation) Matches(model ModelRef) bool {
	return (filter.Provider == "" || filter.Provider == model.ID.Provider) &&
		(filter.Model == nil || *filter.Model == model.ID) &&
		(filter.Profile == nil || *filter.Profile == model.Profile)
}

func (key driverCacheKey) singleflightKey() string {
	return fmt.Sprintf(
		"%q|%q|%q|%q|%d",
		key.model.ID.Provider,
		key.model.ID.Name,
		key.model.Profile,
		key.operation,
		key.generation,
	)
}

func unsupportedOperation(
	model ModelRef,
	operation Operation,
	name string,
) error {
	return NewError(
		UnsupportedOperation,
		operation,
		"",
		fmt.Errorf("model %q does not implement %s", model.ID.Name, name),
	)
}

func cacheTypeError(operation Operation) error {
	return NewError(
		CompilerContractViolation,
		operation,
		"",
		fmt.Errorf("runtime driver cache contains the wrong type"),
	)
}
