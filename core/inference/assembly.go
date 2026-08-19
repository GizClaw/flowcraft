package inference

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Assembly is the inference resource value: the provider definitions
// collected from its many deps, frozen into a provider registry on
// first use, and the execution entry for Generate / Embed.
type Assembly struct {
	providers map[string]ProviderDefinition

	once        sync.Once
	registry    map[string]providerEntry
	registryErr error
}

// Providers returns the assembly's provider definitions sorted by
// provider ID.
func (a *Assembly) Providers() []ProviderDefinition {
	ids := make([]string, 0, len(a.providers))
	for id := range a.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	providers := make([]ProviderDefinition, 0, len(ids))
	for _, id := range ids {
		providers = append(providers, a.providers[id])
	}
	return providers
}

// ExtensionDecoders returns every configured provider's extension
// decoders keyed "provider/extension" — the registry shape handed to
// DecodeExtensions. Decoders are provider-carried, so the available
// menu tracks the providers actually configured in the deployment
// instead of host-side registration.
func (a *Assembly) ExtensionDecoders() map[string]ExtensionDecoder {
	decoders := make(map[string]ExtensionDecoder)
	for providerID, def := range a.providers {
		for extensionID, decoder := range def.ExtensionDecoders {
			decoders[providerID+"/"+extensionID] = decoder
		}
	}
	return decoders
}

// registryView freezes the provider catalog into the execution
// registry on first use (idempotent, concurrent-safe).
func (a *Assembly) registryView() (map[string]providerEntry, error) {
	a.once.Do(func() {
		definitions := make([]ProviderDefinition, 0, len(a.providers))
		for _, def := range a.providers {
			definitions = append(definitions, def)
		}
		sort.Slice(definitions, func(i, j int) bool {
			return definitions[i].ID < definitions[j].ID
		})
		a.registry, a.registryErr = buildRegistry(definitions)
	})
	return a.registry, a.registryErr
}

// Validate freezes the provider registry, surfacing catalog errors
// (duplicate providers, invalid models, missing openers) up front.
func (a *Assembly) Validate() error {
	_, err := a.registryView()
	return err
}

// Models returns every model descriptor across providers, sorted by
// provider then model name.
func (a *Assembly) Models() []ModelDescriptor {
	registry, err := a.registryView()
	if err != nil {
		return nil
	}
	descriptors := make([]ModelDescriptor, 0)
	for _, entry := range registry {
		for _, model := range entry.definition.Models {
			descriptors = append(descriptors, model.Descriptor.Clone())
		}
	}
	sort.Slice(descriptors, func(i, j int) bool {
		if descriptors[i].ID.Provider != descriptors[j].ID.Provider {
			return descriptors[i].ID.Provider < descriptors[j].ID.Provider
		}
		return descriptors[i].ID.Name < descriptors[j].ID.Name
	})
	return descriptors
}

// InspectModel returns the descriptor for a concrete model reference.
func (a *Assembly) InspectModel(ref ModelRef) (ModelDescriptor, error) {
	_, model, err := a.lookupEntry(ref, "")
	if err != nil {
		return ModelDescriptor{}, err
	}
	return model.Descriptor.Clone(), nil
}

// Generate executes one unary generate request against the model
// addressed by ref.
func (a *Assembly) Generate(
	ctx context.Context,
	ref ModelRef,
	req GenerateRequest,
) (GenerateResponse, error) {
	operations, err := a.openGenerate(ctx, ref)
	if err != nil {
		return GenerateResponse{}, err
	}
	if operations.Unary == nil {
		return GenerateResponse{}, NewError(
			UnsupportedOperation, OperationGenerate, "",
			fmt.Errorf("model %q has no unary generate driver", ref.ID.Name))
	}
	return operations.Unary.Execute(ctx, ref, req)
}

// ExplainGenerate runs the compiler for a unary generate request
// without provider I/O.
func (a *Assembly) ExplainGenerate(
	ctx context.Context,
	ref ModelRef,
	req GenerateRequest,
) (Explanation, error) {
	operations, err := a.openGenerate(ctx, ref)
	if err != nil {
		return Explanation{}, err
	}
	if operations.Unary == nil {
		return Explanation{}, NewError(
			UnsupportedOperation, OperationGenerate, "",
			fmt.Errorf("model %q has no unary generate driver", ref.ID.Name))
	}
	return operations.Unary.Explain(ctx, ref, req)
}

// GenerateStream opens a finite generate stream against the model
// addressed by ref.
func (a *Assembly) GenerateStream(
	ctx context.Context,
	ref ModelRef,
	req GenerateRequest,
) (GenerateStream, error) {
	operations, err := a.openGenerate(ctx, ref)
	if err != nil {
		return nil, err
	}
	if operations.Stream == nil {
		return nil, NewError(
			UnsupportedOperation, OperationGenerate, "",
			fmt.Errorf("model %q has no streaming generate driver", ref.ID.Name))
	}
	return operations.Stream.Stream(ctx, ref, req)
}

// ExplainGenerateStream runs the compiler for a generate stream
// without provider I/O.
func (a *Assembly) ExplainGenerateStream(
	ctx context.Context,
	ref ModelRef,
	req GenerateRequest,
) (Explanation, error) {
	operations, err := a.openGenerate(ctx, ref)
	if err != nil {
		return Explanation{}, err
	}
	if operations.Stream == nil {
		return Explanation{}, NewError(
			UnsupportedOperation, OperationGenerate, "",
			fmt.Errorf("model %q has no streaming generate driver", ref.ID.Name))
	}
	return operations.Stream.Explain(ctx, ref, req)
}

// Embed executes one embedding request against the model addressed by
// ref.
func (a *Assembly) Embed(
	ctx context.Context,
	ref ModelRef,
	req EmbedRequest,
) (EmbedResponse, error) {
	driver, err := a.openEmbed(ctx, ref)
	if err != nil {
		return EmbedResponse{}, err
	}
	return driver.Execute(ctx, ref, req)
}

// ExplainEmbed runs the compiler for an embedding request without
// provider I/O.
func (a *Assembly) ExplainEmbed(
	ctx context.Context,
	ref ModelRef,
	req EmbedRequest,
) (Explanation, error) {
	driver, err := a.openEmbed(ctx, ref)
	if err != nil {
		return Explanation{}, err
	}
	return driver.Explain(ctx, ref, req)
}

// Transcribe executes one unary transcription request against the model
// addressed by ref.
func (a *Assembly) Transcribe(
	ctx context.Context,
	ref ModelRef,
	req TranscriptionRequest,
) (TranscriptionResponse, error) {
	operations, err := a.openTranscribe(ctx, ref)
	if err != nil {
		return TranscriptionResponse{}, err
	}
	if operations.Unary == nil {
		return TranscriptionResponse{}, NewError(
			UnsupportedOperation, OperationTranscription, "",
			fmt.Errorf("model %q has no unary transcription driver", ref.ID.Name),
		)
	}
	return operations.Unary.Execute(ctx, ref, req)
}

// ExplainTranscribe runs the compiler for a unary transcription request
// without provider I/O.
func (a *Assembly) ExplainTranscribe(
	ctx context.Context,
	ref ModelRef,
	req TranscriptionRequest,
) (Explanation, error) {
	operations, err := a.openTranscribe(ctx, ref)
	if err != nil {
		return Explanation{}, err
	}
	if operations.Unary == nil {
		return Explanation{}, NewError(
			UnsupportedOperation, OperationTranscription, "",
			fmt.Errorf("model %q has no unary transcription driver", ref.ID.Name),
		)
	}
	return operations.Unary.Explain(ctx, ref, req)
}

// TranscribeSession opens a duplex transcription session against the model
// addressed by ref. Audio is fed through the returned session after open.
func (a *Assembly) TranscribeSession(
	ctx context.Context,
	ref ModelRef,
	req TranscriptionSessionRequest,
) (TranscriptionSession, error) {
	operations, err := a.openTranscribe(ctx, ref)
	if err != nil {
		return nil, err
	}
	if operations.Session == nil {
		return nil, NewError(
			UnsupportedOperation, OperationTranscription, "",
			fmt.Errorf("model %q has no transcription session driver", ref.ID.Name),
		)
	}
	return operations.Session.Open(ctx, ref, req)
}

// ExplainTranscribeSession compiles a transcription session request without
// provider I/O.
func (a *Assembly) ExplainTranscribeSession(
	ctx context.Context,
	ref ModelRef,
	req TranscriptionSessionRequest,
) (Explanation, error) {
	operations, err := a.openTranscribe(ctx, ref)
	if err != nil {
		return Explanation{}, err
	}
	if operations.Session == nil {
		return Explanation{}, NewError(
			UnsupportedOperation, OperationTranscription, "",
			fmt.Errorf("model %q has no transcription session driver", ref.ID.Name),
		)
	}
	return operations.Session.Explain(ctx, ref, req)
}

func (a *Assembly) openGenerate(
	ctx context.Context,
	ref ModelRef,
) (GenerateOperations, error) {
	entry, model, err := a.lookupEntry(ref, OperationGenerate)
	if err != nil {
		return GenerateOperations{}, err
	}
	if model.Openers.Generate == nil {
		return GenerateOperations{}, NewError(
			UnsupportedOperation, OperationGenerate, "",
			fmt.Errorf("model %q has no generate openers", ref.ID.Name))
	}
	if err := entry.checkProfile(ref, OperationGenerate); err != nil {
		return GenerateOperations{}, err
	}
	return model.Openers.Generate(ctx, ref)
}

func (a *Assembly) openEmbed(
	ctx context.Context,
	ref ModelRef,
) (EmbedDriver, error) {
	entry, model, err := a.lookupEntry(ref, OperationEmbed)
	if err != nil {
		return nil, err
	}
	if model.Openers.Embed == nil {
		return nil, NewError(
			UnsupportedOperation, OperationEmbed, "",
			fmt.Errorf("model %q has no embed openers", ref.ID.Name))
	}
	if err := entry.checkProfile(ref, OperationEmbed); err != nil {
		return nil, err
	}
	return model.Openers.Embed(ctx, ref)
}

func (a *Assembly) openTranscribe(
	ctx context.Context,
	ref ModelRef,
) (TranscribeOperations, error) {
	entry, model, err := a.lookupEntry(ref, OperationTranscription)
	if err != nil {
		return TranscribeOperations{}, err
	}
	if model.Openers.Transcribe == nil {
		return TranscribeOperations{}, NewError(
			UnsupportedOperation, OperationTranscription, "",
			fmt.Errorf("model %q has no transcribe openers", ref.ID.Name))
	}
	if err := entry.checkProfile(ref, OperationTranscription); err != nil {
		return TranscribeOperations{}, err
	}
	return model.Openers.Transcribe(ctx, ref)
}

func (a *Assembly) lookupEntry(
	ref ModelRef,
	operation Operation,
) (providerEntry, ModelImplementation, error) {
	registry, err := a.registryView()
	if err != nil {
		return providerEntry{}, ModelImplementation{},
			errdefs.Validationf("inference assembly: %v", err)
	}
	if err := ref.Validate(); err != nil {
		return providerEntry{}, ModelImplementation{},
			NewError(InvalidRequest, operation, "", err)
	}
	entry, ok := registry[ref.ID.Provider]
	if !ok {
		return providerEntry{}, ModelImplementation{}, NewError(
			UnknownProvider, operation, "",
			fmt.Errorf("provider %q", ref.ID.Provider))
	}
	index, ok := entry.models[ref.ID.Name]
	if !ok {
		return providerEntry{}, ModelImplementation{}, NewError(
			UnknownModel, operation, "",
			fmt.Errorf("model %q", ref.ID.Name))
	}
	return entry, entry.definition.Models[index], nil
}

func (entry providerEntry) checkProfile(
	ref ModelRef,
	operation Operation,
) error {
	profile, ok := entry.profiles[ref.Profile]
	if !ok {
		return NewError(
			UnknownProfile, operation, "",
			fmt.Errorf("profile %q", ref.Profile))
	}
	if !profile.allows(operation) {
		return NewError(
			UnsupportedOperation, operation, "",
			fmt.Errorf("profile %q does not allow %s", ref.Profile, operation))
	}
	return nil
}
