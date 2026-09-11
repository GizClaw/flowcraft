package inference

import (
	"context"
	"errors"
	"fmt"
)

// The prepare*Call helpers turn an opened driver set plus one request into a
// prepared attempt. They are the shared tail of Assembly's execution methods
// and Binding's Prepare methods: the two differ only in whether they resolve
// the drivers first, so the operation-availability check and the shape of the
// prepared attempt stay in one place.

func prepareGenerateCall(
	ctx context.Context,
	operations GenerateOperations,
	ref ModelRef,
	request GenerateRequest,
) (*Prepared[GenerateResponse], error) {
	if operations.Unary == nil {
		return nil, NewError(
			UnsupportedOperation, OperationGenerate, "",
			fmt.Errorf("model %q has no unary generate driver", ref.ID.Name))
	}
	return operations.Unary.Prepare(ctx, ref, request)
}

func prepareGenerateStreamCall(
	ctx context.Context,
	operations GenerateOperations,
	ref ModelRef,
	request GenerateRequest,
) (*Prepared[GenerateStream], error) {
	if operations.Stream == nil {
		return nil, NewError(
			UnsupportedOperation, OperationGenerate, "",
			fmt.Errorf("model %q has no streaming generate driver", ref.ID.Name))
	}
	return operations.Stream.PrepareStream(ctx, ref, request)
}

func prepareEmbedCall(
	ctx context.Context,
	driver EmbedDriver,
	ref ModelRef,
	request EmbedRequest,
) (*Prepared[EmbedResponse], error) {
	if isNilValue(driver) {
		return nil, NewError(
			UnsupportedOperation, OperationEmbed, "",
			fmt.Errorf("model %q has no embed driver", ref.ID.Name))
	}
	return driver.Prepare(ctx, ref, request)
}

func prepareTranscribeCall(
	ctx context.Context,
	operations TranscribeOperations,
	ref ModelRef,
	request TranscriptionRequest,
) (*Prepared[TranscriptionResponse], error) {
	if operations.Unary == nil {
		return nil, NewError(
			UnsupportedOperation, OperationTranscription, "",
			fmt.Errorf("model %q has no unary transcription driver", ref.ID.Name))
	}
	return operations.Unary.Prepare(ctx, ref, request)
}

func prepareTranscribeSessionCall(
	ctx context.Context,
	operations TranscribeOperations,
	ref ModelRef,
	request TranscriptionSessionRequest,
) (*Prepared[TranscriptionSession], error) {
	if operations.Session == nil {
		return nil, NewError(
			UnsupportedOperation, OperationTranscription, "",
			fmt.Errorf("model %q has no transcription session driver", ref.ID.Name))
	}
	return operations.Session.PrepareSession(ctx, ref, request)
}

// Prepared is one attempt whose compiler work is already done: the provider
// request is validated, compiled, and ledger-checked, and executing it
// performs provider I/O only. It is what makes preflight and execution share a
// single compilation instead of compiling the same request twice — a routed
// generate call preflights a target (Explain-shaped work, no provider I/O) and
// then executes the very compilation it preflighted.
//
// A Prepared is single-use and belongs to the caller that produced it; it holds
// the compiled wire value until executed, and hosts that never execute it
// simply drop it.
//
// Execute performs a provider round trip every time it is called; what is
// reused is the compilation, not the call. Executing one handle twice therefore
// calls the provider twice — billed twice, and for a stream, two streams
// opened. That is deliberate: re-executing is how a caller retries an attempt
// without recompiling it. Callers that must not duplicate a call should treat
// the handle as single-use and drop it.
type Prepared[Result any] struct {
	model       ModelRef
	operation   Operation
	explanation Explanation
	run         func(context.Context) (Result, error)
}

// newPrepared builds a handle over an already-compiled attempt. run must
// perform provider I/O only: everything local has already happened.
func newPrepared[Result any](
	model ModelRef,
	operation Operation,
	report CompileReport,
	run func(context.Context) (Result, error),
) *Prepared[Result] {
	return &Prepared[Result]{
		model:     model,
		operation: operation,
		explanation: Explanation{
			Model:     model,
			Operation: operation,
			Decisions: append([]Decision(nil), report.Decisions...),
		},
		run: run,
	}
}

// Explanation reports the compiler decisions this preparation produced,
// including any field the compiler dropped with a reason. It is a snapshot:
// later use of the handle does not change it.
func (p *Prepared[Result]) Explanation() Explanation {
	if p == nil {
		return Explanation{}
	}
	explanation := p.explanation
	explanation.Decisions = append([]Decision(nil), p.explanation.Decisions...)
	return explanation
}

// Model returns the exact model reference this attempt was prepared against,
// including the credential profile that resolved it.
func (p *Prepared[Result]) Model() ModelRef {
	if p == nil {
		return ModelRef{}
	}
	return p.model
}

// Execute runs the prepared attempt. It never compiles: a failure past this
// point is a provider failure or a response contract violation.
func (p *Prepared[Result]) Execute(ctx context.Context) (Result, error) {
	var zero Result
	if p == nil || p.run == nil {
		return zero, NewError(
			InvalidRequest,
			"",
			"",
			errors.New("prepared attempt is empty"),
		)
	}
	return p.run(ctx)
}
