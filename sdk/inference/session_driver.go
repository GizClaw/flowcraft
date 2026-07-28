package inference

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type transcriptionSessionDriver[Wire any] struct {
	pipeline *pipeline[
		TranscriptionSessionConfig,
		Wire,
		TranscriptionSession,
		TranscriptionSession,
	]
}

func (*transcriptionSessionDriver[Wire]) inferenceTranscriptionSessionDriver() {}

func (d *transcriptionSessionDriver[Wire]) Explain(
	ctx context.Context,
	model ModelRef,
	config TranscriptionSessionConfig,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, config)
}

func (d *transcriptionSessionDriver[Wire]) Open(
	ctx context.Context,
	model ModelRef,
	config TranscriptionSessionConfig,
) (TranscriptionSession, Metadata, error) {
	session, report, err := d.pipeline.execute(ctx, model, config)
	if err != nil {
		return nil, Metadata{}, err
	}
	return session, report.Metadata(model), nil
}

type realtimeDriver[ConfigWire, InputWire, RawEvent any] struct {
	pipeline *pipeline[
		RealtimeConfig,
		ConfigWire,
		ProviderRealtimeSession[InputWire, RawEvent],
		ProviderRealtimeSession[InputWire, RawEvent],
	]
	input       *realtimeInputCompiler[InputWire]
	decodeEvent Decoder[RawEvent, RealtimeEvent]
}

func (*realtimeDriver[ConfigWire, InputWire, RawEvent]) inferenceRealtimeDriver() {}

func (d *realtimeDriver[ConfigWire, InputWire, RawEvent]) Explain(
	ctx context.Context,
	model ModelRef,
	config RealtimeConfig,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, config)
}

func (d *realtimeDriver[ConfigWire, InputWire, RawEvent]) Open(
	ctx context.Context,
	model ModelRef,
	config RealtimeConfig,
) (RealtimeSession, Metadata, error) {
	provider, report, err := d.pipeline.execute(ctx, model, config)
	if err != nil {
		return nil, Metadata{}, err
	}
	return &compiledRealtimeSession[InputWire, RawEvent]{
		model:       model,
		provider:    provider,
		input:       d.input,
		decodeEvent: d.decodeEvent,
	}, report.Metadata(model), nil
}

type realtimeInputCompiler[Wire any] struct {
	compile Compiler[RealtimeInput, Wire]
}

func bindRealtimeInputCompiler[Wire any](
	compile Compiler[RealtimeInput, Wire],
) (*realtimeInputCompiler[Wire], error) {
	if compile == nil {
		return nil, errdefs.Validationf("realtime input compiler is required")
	}
	if typeContains(
		reflect.TypeFor[Wire](),
		reflect.TypeFor[RealtimeInput](),
		make(map[reflect.Type]bool),
	) {
		return nil, errdefs.Validationf(
			"provider wire type must not contain the canonical realtime input type",
		)
	}
	return &realtimeInputCompiler[Wire]{compile: compile}, nil
}

func (c *realtimeInputCompiler[Wire]) prepare(
	ctx context.Context,
	model ModelRef,
	input RealtimeInput,
) (Compiled[Wire], error) {
	var zero Compiled[Wire]
	if isNilValue(input) {
		return zero, NewError(
			InvalidRequest,
			OperationRealtime,
			"",
			errors.New("realtime input is required"),
		)
	}
	if err := input.Validate(); err != nil {
		return zero, NewError(InvalidRequest, OperationRealtime, "", err)
	}
	active := append([]FieldID(nil), input.ActiveFields()...)
	snapshot := input.Clone()
	if isNilValue(snapshot) || !sameFieldSet(active, snapshot.ActiveFields()) {
		return zero, contractViolation(
			OperationRealtime,
			"",
			"realtime input clone changed active fields",
		)
	}
	compiled, err := c.compile(ctx, model, snapshot)
	if err != nil {
		classified := errdefs.FromContext(err)
		if errdefs.IsAborted(classified) || errdefs.IsTimeout(classified) {
			return compiled, NewError(
				OperationInterrupted,
				OperationRealtime,
				"",
				classified,
			)
		}
		if reportErr := compiled.Report.ValidateFailure(
			OperationRealtime,
			active,
		); reportErr != nil {
			return compiled, reportErr
		}
		var inferenceErr *Error
		if !errors.As(err, &inferenceErr) ||
			inferenceErr.Operation != OperationRealtime ||
			inferenceErr.Field == "" ||
			!inferenceErr.Kind.isCompilerRejection() ||
			!compiled.Report.Rejects(inferenceErr.Field) {
			return compiled, contractViolation(
				OperationRealtime,
				"",
				"realtime input compiler returned an invalid structured error",
			)
		}
		return compiled, inferenceErr
	}
	if err := compiled.Report.ValidateSuccess(OperationRealtime, active); err != nil {
		return compiled, err
	}
	return compiled, nil
}

type compiledRealtimeSession[WireInput, RawEvent any] struct {
	model       ModelRef
	provider    ProviderRealtimeSession[WireInput, RawEvent]
	input       *realtimeInputCompiler[WireInput]
	decodeEvent Decoder[RawEvent, RealtimeEvent]

	reportsMu sync.RWMutex
	reports   []CompileReport
}

func (s *compiledRealtimeSession[WireInput, RawEvent]) Send(
	ctx context.Context,
	input RealtimeInput,
) error {
	compiled, err := s.input.prepare(ctx, s.model, input)
	if compiled.Report.Operation != "" {
		s.reportsMu.Lock()
		s.reports = append(s.reports, compiled.Report.Clone())
		s.reportsMu.Unlock()
	}
	if err != nil {
		return err
	}
	if err := s.provider.Send(ctx, compiled.Wire); err != nil {
		return newProviderError(OperationRealtime, s.model.ID.Provider, err)
	}
	return nil
}

func (s *compiledRealtimeSession[WireInput, RawEvent]) inputReports() []CompileReport {
	s.reportsMu.RLock()
	defer s.reportsMu.RUnlock()
	reports := make([]CompileReport, len(s.reports))
	for index, report := range s.reports {
		reports[index] = report.Clone()
	}
	return reports
}

func (s *compiledRealtimeSession[WireInput, RawEvent]) Next(
	ctx context.Context,
) (RealtimeEvent, error) {
	raw, err := s.provider.Next(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, newProviderError(OperationRealtime, s.model.ID.Provider, err)
	}
	event, err := s.decodeEvent(ctx, raw)
	if err != nil {
		return nil, NewError(InvalidProviderResponse, OperationRealtime, "", err)
	}
	if isNilValue(event) {
		return nil, NewError(
			InvalidProviderResponse,
			OperationRealtime,
			"",
			errors.New("provider decoded a nil realtime event"),
		)
	}
	if err := event.Validate(); err != nil {
		return nil, NewError(InvalidProviderResponse, OperationRealtime, "", err)
	}
	return event, nil
}

func (s *compiledRealtimeSession[WireInput, RawEvent]) CancelResponse(
	ctx context.Context,
) error {
	if err := s.provider.CancelResponse(ctx); err != nil {
		return newProviderError(OperationRealtime, s.model.ID.Provider, err)
	}
	return nil
}

func (s *compiledRealtimeSession[WireInput, RawEvent]) Close() error {
	if err := s.provider.Close(); err != nil {
		return newProviderError(OperationRealtime, s.model.ID.Provider, err)
	}
	return nil
}
