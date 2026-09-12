package inference

import (
	"context"
	"fmt"
)

// Binding is the opened driver set of one model reference: every operation the
// model declares, resolved once.
//
// Binding exists because opening is deployment-scoped work — resolving
// credentials and constructing provider clients — while the default
// Assembly.Generate/... path resolves it per call. Hosts that keep a model for
// the life of a process (a graph node, a long-lived session, a server) bind
// once and reuse the handle; nothing is cached behind their back, and a
// deployment never needs credentials at build time, only at the first bind.
//
// A Binding is immutable once returned and safe for concurrent use, because the
// drivers it holds are (the provider SPI requires it). Bind again to pick up
// rotated credentials: the old handle keeps working until it is dropped.
type Binding struct {
	ref        ModelRef
	descriptor ModelDescriptor
	generate   GenerateOperations
	embed      EmbedDriver
	transcribe TranscribeOperations
	// unbound carries, per operation the reference's credential profile does
	// not allow, the rejection the per-call path returns. Binding one
	// operation the profile refuses must not fail the whole handle: a model
	// that declares several operations stays usable for the operations the
	// profile does allow, exactly as Assembly.Generate/Embed/Transcribe are.
	unbound map[Operation]error
}

// Bind opens every operation the addressed model declares. The reference must
// name a configured provider, model, and credential profile. The
// request-path rules (operation support, profile allow-list) are enforced
// here and per operation: a binding that returns without error can serve
// every operation its profile allows, and a Prepare for a refused operation
// reports the same error the per-call path does.
func (a *Assembly) Bind(ctx context.Context, ref ModelRef) (*Binding, error) {
	entry, model, err := a.lookupEntry(ref, "")
	if err != nil {
		return nil, err
	}
	binding := &Binding{ref: ref, descriptor: model.Descriptor.Clone()}
	for _, operation := range model.Descriptor.Operations {
		if err := entry.checkProfile(ref, operation); err != nil {
			if binding.unbound == nil {
				binding.unbound = make(map[Operation]error, 1)
			}
			binding.unbound[operation] = err
			continue
		}
		switch operation {
		case OperationGenerate:
			binding.generate, err = model.Openers.Generate(ctx, ref)
		case OperationEmbed:
			binding.embed, err = model.Openers.Embed(ctx, ref)
		case OperationTranscription:
			binding.transcribe, err = model.Openers.Transcribe(ctx, ref)
		default:
			err = fmt.Errorf("unsupported operation %q", operation)
		}
		if err != nil {
			return nil, err
		}
	}
	return binding, nil
}

// Ref returns the exact model reference this binding was opened for, including
// the credential profile that resolved it.
func (b *Binding) Ref() ModelRef {
	if b == nil {
		return ModelRef{}
	}
	return b.ref
}

// Descriptor returns the model's declaration as it was frozen at bind time.
func (b *Binding) Descriptor() ModelDescriptor {
	if b == nil {
		return ModelDescriptor{}
	}
	return b.descriptor.Clone()
}

// PrepareGenerate compiles one unary request against the bound driver.
func (b *Binding) PrepareGenerate(
	ctx context.Context,
	request GenerateRequest,
) (*Prepared[GenerateResponse], error) {
	if err := b.operationRejection(OperationGenerate); err != nil {
		return nil, err
	}
	if b == nil || b.generate.Unary == nil {
		return nil, NewError(
			UnsupportedOperation, OperationGenerate, "",
			fmt.Errorf("model %q has no unary generate driver", b.refName()))
	}
	if err := checkGenerateDeclaration(b.descriptor, request); err != nil {
		return nil, err
	}
	prepared, err := b.generate.Unary.Prepare(ctx, b.ref, request)
	if err != nil {
		return nil, err
	}
	return instrumentGenerateCall(prepared), nil
}

// PrepareGenerateStream compiles one streaming request against the bound
// driver.
func (b *Binding) PrepareGenerateStream(
	ctx context.Context,
	request GenerateRequest,
) (*Prepared[GenerateStream], error) {
	if err := b.operationRejection(OperationGenerate); err != nil {
		return nil, err
	}
	if b == nil || b.generate.Stream == nil {
		return nil, NewError(
			UnsupportedOperation, OperationGenerate, "",
			fmt.Errorf("model %q has no streaming generate driver", b.refName()))
	}
	if err := checkGenerateDeclaration(b.descriptor, request); err != nil {
		return nil, err
	}
	prepared, err := b.generate.Stream.PrepareStream(ctx, b.ref, request)
	if err != nil {
		return nil, err
	}
	return instrumentGenerateStreamCall(prepared), nil
}

// PrepareEmbed compiles one embedding request against the bound driver.
func (b *Binding) PrepareEmbed(
	ctx context.Context,
	request EmbedRequest,
) (*Prepared[EmbedResponse], error) {
	if err := b.operationRejection(OperationEmbed); err != nil {
		return nil, err
	}
	if b == nil || b.embed == nil {
		return nil, NewError(
			UnsupportedOperation, OperationEmbed, "",
			fmt.Errorf("model %q has no embed driver", b.refName()))
	}
	prepared, err := b.embed.Prepare(ctx, b.ref, request)
	if err != nil {
		return nil, err
	}
	return instrumentEmbedCall(prepared), nil
}

// PrepareTranscribe compiles one whole-file transcription request against the
// bound driver.
func (b *Binding) PrepareTranscribe(
	ctx context.Context,
	request TranscriptionRequest,
) (*Prepared[TranscriptionResponse], error) {
	if err := b.operationRejection(OperationTranscription); err != nil {
		return nil, err
	}
	if b == nil || b.transcribe.Unary == nil {
		return nil, NewError(
			UnsupportedOperation, OperationTranscription, "",
			fmt.Errorf("model %q has no unary transcription driver", b.refName()))
	}
	prepared, err := b.transcribe.Unary.Prepare(ctx, b.ref, request)
	if err != nil {
		return nil, err
	}
	return instrumentTranscribeCall(prepared), nil
}

// PrepareTranscribeSession compiles one duplex session request against the
// bound driver.
func (b *Binding) PrepareTranscribeSession(
	ctx context.Context,
	request TranscriptionSessionRequest,
) (*Prepared[TranscriptionSession], error) {
	if err := b.operationRejection(OperationTranscription); err != nil {
		return nil, err
	}
	if b == nil || b.transcribe.Session == nil {
		return nil, NewError(
			UnsupportedOperation, OperationTranscription, "",
			fmt.Errorf("model %q has no transcription session driver", b.refName()))
	}
	prepared, err := b.transcribe.Session.PrepareSession(ctx, b.ref, request)
	if err != nil {
		return nil, err
	}
	return instrumentTranscribeSessionCall(prepared), nil
}

func (b *Binding) refName() string {
	if b == nil {
		return ""
	}
	return b.ref.ID.Name
}

// operationRejection reports the profile rejection recorded for operation
// when the credential profile does not allow it, and nil otherwise. It is
// what keeps a refused operation reporting the per-call path's error instead
// of the "no driver" error an unbound operation would otherwise produce.
func (b *Binding) operationRejection(operation Operation) error {
	if b == nil {
		return nil
	}
	return b.unbound[operation]
}
