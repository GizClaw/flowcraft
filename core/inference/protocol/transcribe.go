package protocol

import (
	"context"
	"errors"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// BindTranscriber assembles one transcription pipeline. It is the generation
// pipeline minus streaming — a transcript is one answer — and reuses the same
// front half: validate, compile, prove the compilation accounted for every
// active field, classify the failures.
//
// A driver that returns nil for compile, transport or decode is rejected here
// rather than at the first call, because a pipeline missing a stage cannot be
// repaired at runtime.
func BindTranscriber[Wire, Raw any](
	providerID string,
	compile Compiler[inference.TranscribeRequest, Wire],
	transport Transport[inference.TranscribeRequest, Wire, Raw],
	decode Decoder[inference.TranscribeRequest, Raw, inference.TranscribeResponse],
) (inference.Transcriber, error) {
	switch {
	case providerID == "":
		return nil, errors.New("bind transcriber: provider id is required")
	case compile == nil:
		return nil, errors.New("bind transcriber: compiler is required")
	case transport == nil:
		return nil, errors.New("bind transcriber: transport is required")
	case decode == nil:
		return nil, errors.New("bind transcriber: decoder is required")
	}
	return &transcribeDriver[Wire, Raw]{
		providerID: providerID,
		compile:    compile,
		transport:  transport,
		decode:     decode,
	}, nil
}

type transcribeDriver[Wire, Raw any] struct {
	providerID string
	compile    Compiler[inference.TranscribeRequest, Wire]
	transport  Transport[inference.TranscribeRequest, Wire, Raw]
	decode     Decoder[inference.TranscribeRequest, Raw, inference.TranscribeResponse]
}

// Transcribe validates, compiles, calls and decodes one request.
func (d *transcribeDriver[Wire, Raw]) Transcribe(
	ctx context.Context,
	request inference.TranscribeRequest,
) (inference.TranscribeResponse, error) {
	request.Extensions = request.Extensions.ForProvider(d.providerID)

	compiled, err := prepare(ctx, d.providerID, model.OperationTranscription, d.compile, request, ShapeUnary)
	if err != nil {
		return inference.TranscribeResponse{}, err
	}
	raw, err := d.transport(ctx, request, compiled.Wire)
	if err != nil {
		return inference.TranscribeResponse{}, transportError(ctx, model.OperationTranscription, err)
	}
	response, err := d.decode(ctx, request, raw)
	if err != nil {
		return inference.TranscribeResponse{}, decodeError(model.OperationTranscription, err)
	}
	response.Warnings = joinWarnings(compiled.Warnings, response.Warnings)
	if err := response.ValidateFor(request); err != nil {
		return inference.TranscribeResponse{}, err
	}
	return response, nil
}

// CheckCatalogTranscription reports whether a catalog's declarations can be
// served by the seams the host actually holds.
//
// A model that declares transcription while no Transcriber is bound describes a
// capability that cannot be called; the failure belongs at construction, where
// a deployment can be fixed, not at the first request.
func CheckCatalogTranscription(catalog inference.Catalog, transcriber inference.Transcriber) error {
	if catalog == nil {
		return nil
	}
	for _, descriptor := range catalog.Models() {
		if !slices.Contains(descriptor.Operations, model.OperationTranscription) {
			continue
		}
		if transcriber == nil {
			return inference.NewError(
				inference.UnsupportedOperation,
				model.OperationTranscription,
				inference.FieldModel,
				errors.New("model "+descriptor.ID.Name+" declares transcription but no transcriber is bound"),
			)
		}
	}
	return nil
}
